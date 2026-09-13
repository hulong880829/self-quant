#include "oms/api/oms_api.h"

#include <algorithm>
#include <atomic>
#include <bit>
#include <cerrno>
#include <chrono>
#include <limits>
#include <mutex>
#include <stdexcept>
#include <string_view>
#include <system_error>
#include <thread>
#include <utility>
#include <vector>

#include <sys/epoll.h>
#include <time.h>
#include <unistd.h>

#include "oms/exchange/adapter_router.h"
#include "oms/exchange/fake_trade_adapter.h"
#include "oms/execution_directory.h"
#include "oms/order_table.h"
#include "oms/runtime/deadline_scheduler.h"
#include "oms/runtime/event_notifier.h"
#include "oms/runtime/fixed_spsc_ring.h"
#include "oms/runtime/runtime_command.h"
#include "oms/runtime/timer_notifier.h"
#include "oms/state_engine.h"

namespace oms::api {
namespace {

std::uint64_t NowNs() noexcept {
  timespec value{};
  if (::clock_gettime(CLOCK_MONOTONIC, &value) != 0) return 0;
  return static_cast<std::uint64_t>(value.tv_sec) * 1'000'000'000ULL +
         static_cast<std::uint64_t>(value.tv_nsec);
}

bool ValidCapacity(std::uint32_t value) noexcept {
  return value >= 2 && std::has_single_bit(value);
}

bool IsTerminal(OrderStatus status) noexcept {
  return status == OrderStatus::Filled || status == OrderStatus::Canceled ||
         status == OrderStatus::Rejected || status == OrderStatus::Expired;
}

bool InitialRoute(const InstrumentInit& source,
                  ResolvedInstrument& routing) noexcept {
  const auto& instrument = source.instrument;
  routing.kind = instrument.venue == utils::md::Venue::Polymarket
                     ? ExecutionRouteKind::Polymarket
                     : ExecutionRouteKind::Crypto;
  routing.venue = static_cast<std::uint8_t>(instrument.venue);
  routing.product_type = static_cast<std::uint8_t>(instrument.product_type);
  routing.price_scale = instrument.price_scale;
  routing.quantity_scale = instrument.quantity_scale;
  routing.catalog_revision = 1;
  routing.tick_size = instrument.tick_size;
  routing.lot_size = instrument.lot_size;
  if (routing.kind == ExecutionRouteKind::Polymarket) {
    routing.signature_type = source.polymarket_signature_type;
    routing.negative_risk = source.polymarket_negative_risk;
    routing.outcome = source.polymarket_outcome;
    routing.taker_delay_ms = source.polymarket_taker_delay_ms;
    routing.minimum_order_size =
        std::max<std::int64_t>(1, source.minimum_order_size);
    routing.polymarket.condition_id = source.polymarket_condition_id;
    routing.polymarket.token_id = source.polymarket_token_id;
    return true;
  }

  const auto venue_end =
      std::find(instrument.venue_symbol.begin(), instrument.venue_symbol.end(),
                '\0');
  std::string_view symbol(
      instrument.venue_symbol.data(),
      static_cast<std::size_t>(venue_end - instrument.venue_symbol.begin()));
  if (symbol.empty()) {
    const auto key_end =
        std::find(instrument.instrument_key.begin(),
                  instrument.instrument_key.end(), '\0');
    const std::string_view key(
        instrument.instrument_key.data(),
        static_cast<std::size_t>(key_end - instrument.instrument_key.begin()));
    const std::size_t first = key.find(':');
    const std::size_t second =
        first == std::string_view::npos ? first : key.find(':', first + 1U);
    const std::size_t third =
        second == std::string_view::npos ? second : key.find(':', second + 1U);
    if (second != std::string_view::npos)
      symbol = key.substr(second + 1U,
                          third == std::string_view::npos
                              ? std::string_view::npos
                              : third - second - 1U);
  }
  if (symbol.empty() || symbol.size() > routing.crypto.venue_symbol.value.size())
    return false;
  std::copy(symbol.begin(), symbol.end(),
            routing.crypto.venue_symbol.value.begin());
  routing.crypto.venue_symbol.length =
      static_cast<std::uint16_t>(symbol.size());
  return true;
}

int ToWaitMilliseconds(std::uint64_t due, std::uint64_t now,
                       int requested) noexcept {
  if (due <= now) return 0;
  const std::uint64_t delta = due - now;
  const std::uint64_t rounded = (delta + 999'999ULL) / 1'000'000ULL;
  const int timer_wait =
      rounded > static_cast<std::uint64_t>(std::numeric_limits<int>::max())
          ? std::numeric_limits<int>::max()
          : static_cast<int>(rounded);
  return requested < 0 ? timer_wait : std::min(requested, timer_wait);
}

}  // namespace

struct OmsApi::Impl {
  static constexpr std::uint64_t kCommandToken = 1;
  static constexpr std::uint64_t kTimerToken = 2;

  struct Lane {
    explicit Lane(const LaneConfig& value, bool dedicated)
        : config(value),
          commands(dedicated
                       ? std::make_unique<
                             runtime::FixedSpscRing<runtime::RuntimeCommand>>(
                             value.command_capacity)
                       : nullptr),
          updates(std::make_unique<runtime::FixedSpscRing<RuntimeUpdate>>(
              value.update_capacity)) {}

    LaneConfig config{};
    std::unique_ptr<runtime::FixedSpscRing<runtime::RuntimeCommand>> commands;
    std::unique_ptr<runtime::FixedSpscRing<RuntimeUpdate>> updates;
    runtime::EventNotifier update_notifier;
    std::atomic<std::uint32_t> session_epoch{0};
    std::atomic<std::uint64_t> next_sequence{0};
    std::atomic_flag producer_busy = ATOMIC_FLAG_INIT;
    std::uint64_t owner_sequence{};
    std::atomic<std::uint64_t> command_full_count{0};
    std::atomic<std::uint64_t> update_full_count{0};
    std::atomic<std::uint64_t> starvation_count{0};
    std::atomic<std::uint64_t> enqueue_samples{0};
    std::atomic<std::uint64_t> enqueue_total_ns{0};
    std::atomic<std::uint64_t> enqueue_max_ns{0};
    std::atomic<std::uint64_t> drain_samples{0};
    std::atomic<std::uint64_t> drain_total_ns{0};
    std::atomic<std::uint64_t> drain_max_ns{0};
    std::atomic<std::uint64_t> drain_latest_ns{0};
  };

  struct StagedUpdates {
    RuntimeUpdate values[2]{};
    std::size_t size{};
    static void Order(void* context, const OrderUpdate& value) noexcept {
      auto& self = *static_cast<StagedUpdates*>(context);
      if (self.size >= std::size(self.values)) return;
      auto& update = self.values[self.size++];
      update.kind = RuntimeUpdateKind::Order;
      update.order = value;
    }
    static void Fill(void* context, const FillUpdate& value) noexcept {
      auto& self = *static_cast<StagedUpdates*>(context);
      if (self.size >= std::size(self.values)) return;
      auto& update = self.values[self.size++];
      update.kind = RuntimeUpdateKind::Fill;
      update.fill = value;
    }
    UpdateSink sink() noexcept { return {this, &Order, &Fill}; }
  };

  struct IoRegistration {
    exchange::AsyncIoDriver* driver{};
    int fd{-1};
    std::uint32_t events{};
    std::uint64_t generation{};
    std::uint64_t token{};
    bool seen{};
  };

  Impl(const RuntimeConfig& value, std::span<const InstrumentInit> instruments,
       std::span<const ReplayStep> replay,
       const AdapterRuntimeConfig& adapter_config)
      : config(value),
        adapter_event_budget(
            adapter_config.event_budget == 0 ? 64 : adapter_config.event_budget),
        inline_owner(std::this_thread::get_id()),
        directory(value.instrument_directory_capacity),
        orders(value.order_capacity),
        engine(orders, value.fill_dedup_capacity),
        scheduler(value.deadline_capacity, timer),
        fake(value.adapter_capacities[
                 static_cast<std::size_t>(AdapterCapacitySlot::Fake)]
                 .send_capacity,
             value.adapter_capacities[
                 static_cast<std::size_t>(AdapterCapacitySlot::Fake)]
                 .receive_capacity) {
    lanes.reserve(config.lane_count);
    for (std::uint32_t index = 0; index < config.lane_count; ++index) {
      lanes.push_back(std::make_unique<Lane>(
          config.lanes[index], config.mode == ExecutionMode::DedicatedIo));
      lanes.back()->update_notifier.arm();
    }
    const std::size_t requested_adapters =
        adapter_config.adapters.size() +
        (adapter_config.enable_fake_fallback ? 1U : 0U);
    if (requested_adapters == 0 ||
        requested_adapters > exchange::kMaxTradeAdapters) {
      throw std::invalid_argument("invalid OMS adapter count");
    }
    for (const auto& value_in : instruments) {
      ResolvedInstrument routing{};
      if (!InitialRoute(value_in, routing) ||
          directory.Register(value_in.instrument.instrument_id, routing) !=
              Error::Ok)
        throw std::invalid_argument("invalid OMS instrument");
    }

    gtd_handles.resize(config.deadline_capacity);
    order_deadlines.resize(orders.capacity());
    std::uint64_t due = NowNs();
    for (const ReplayStep& step : replay) {
      due += step.delay_ns;
      const auto control =
          step.control == ReplayControl::Disconnect
              ? exchange::FakeTradeAdapter::Control::Disconnect
              : (step.control == ReplayControl::Reconnect
                     ? exchange::FakeTradeAdapter::Control::Reconnect
                     : exchange::FakeTradeAdapter::Control::Event);
      if (fake.inject(control, step.event, due) != exchange::AdapterResult::Ok)
        throw std::invalid_argument("fake adapter receive capacity exceeded");
    }

    for (exchange::TradeAdapter* adapter : adapter_config.adapters) {
      if (adapter == nullptr ||
          router.add(*adapter) != exchange::AdapterResult::Ok)
        throw std::invalid_argument("invalid OMS adapter registration");
      adapters[adapter_count++] = adapter;
      status_snapshots[adapter_count - 1].store(
          static_cast<std::uint8_t>(adapter->status()),
          std::memory_order_relaxed);
    }
    if (adapter_config.enable_fake_fallback) {
      if (router.add(fake) != exchange::AdapterResult::Ok)
        throw std::invalid_argument("duplicate fake OMS adapter");
      adapters[adapter_count++] = &fake;
      status_snapshots[adapter_count - 1].store(
          static_cast<std::uint8_t>(fake.status()),
          std::memory_order_relaxed);
    }
    account_routes.assign(adapter_config.account_routes.begin(),
                          adapter_config.account_routes.end());
    std::sort(account_routes.begin(), account_routes.end(),
              [](const auto& left, const auto& right) {
                return left.account_id < right.account_id;
              });
    for (std::size_t index = 0; index < account_routes.size(); ++index) {
      if (account_routes[index].account_id == 0 ||
          router.find(account_routes[index].adapter) == nullptr ||
          (index != 0 &&
           account_routes[index - 1].account_id ==
               account_routes[index].account_id))
        throw std::invalid_argument("invalid OMS account route");
    }
    std::size_t descriptor_capacity = 0;
    for (exchange::AsyncIoDriver* driver : adapter_config.io_drivers) {
      if (driver == nullptr ||
          std::find(io_drivers.begin(), io_drivers.end(), driver) !=
              io_drivers.end()) {
        throw std::invalid_argument("invalid OMS I/O driver registration");
      }
      const std::size_t capacity = driver->descriptor_capacity();
      if (capacity >
          static_cast<std::size_t>(std::numeric_limits<int>::max() - 2) -
              descriptor_capacity) {
        throw std::invalid_argument("invalid OMS I/O descriptor capacity");
      }
      descriptor_capacity += capacity;
      io_drivers.push_back(driver);
      io_driver_capacities.push_back(capacity);
    }
    io_descriptors.resize(descriptor_capacity);
    io_registrations.reserve(descriptor_capacity);
    epoll_events.resize(descriptor_capacity + 2);
    for (const InstrumentInit& value_in : instruments) {
      if (router.route(value_in.instrument) == nullptr)
        throw std::invalid_argument("no OMS adapter for instrument");
    }

    epoll_fd = ::epoll_create1(EPOLL_CLOEXEC);
    if (epoll_fd < 0) throw std::system_error(errno, std::generic_category());
    AddFd(command_notifier.fd(), kCommandToken);
    AddFd(timer.fd(), kTimerToken);
    if (config.mode == ExecutionMode::DedicatedIo) {
      worker = std::jthread([this](std::stop_token token) { Run(token); });
    }
  }

  ~Impl() {
    Stop();
    if (epoll_fd >= 0) ::close(epoll_fd);
  }

  void AddFd(int fd, std::uint64_t token) {
    epoll_event event{};
    event.events = EPOLLIN;
    event.data.u64 = token;
    if (::epoll_ctl(epoll_fd, EPOLL_CTL_ADD, fd, &event) != 0)
      throw std::system_error(errno, std::generic_category());
  }

  Lane* FindLane(std::uint32_t id) noexcept {
    for (auto& lane : lanes)
      if (lane->config.lane_id == id) return lane.get();
    return nullptr;
  }
  const Lane* FindLane(std::uint32_t id) const noexcept {
    return const_cast<Impl*>(this)->FindLane(id);
  }

  OrderRecord* EventRecord(const VenueEvent& event) noexcept {
    OrderRecord* record = nullptr;
    if (event.handle.generation != 0) record = orders.Lookup(event.handle);
    if (record == nullptr && event.token.sequence != 0)
      record = orders.Find(event.token);
    if (record == nullptr && event.client_order_id.length != 0)
      record = orders.Find(event.client_order_id);
    if (record == nullptr && event.venue_order_id.length != 0)
      record = orders.Find(event.venue_order_id);
    return record;
  }

  Lane* EventLane(const VenueEvent& event) noexcept {
    if (event.token.sequence != 0) return FindLane(event.token.lane);
    OrderRecord* record = EventRecord(event);
    return record == nullptr ? nullptr : FindLane(record->request.token.lane);
  }

  bool HasRoom(const Lane& lane, std::size_t count) noexcept {
    return lane.updates->capacity() - lane.updates->depth() >= count;
  }

  static Error AdapterError(exchange::AdapterResult result) noexcept {
    switch (result) {
      case exchange::AdapterResult::Ok:
        return Error::Ok;
      case exchange::AdapterResult::WouldBlock:
        return Error::QueueFull;
      case exchange::AdapterResult::NotReady:
        return Error::NotReady;
      case exchange::AdapterResult::InvalidArgument:
        return Error::InvalidArgument;
      case exchange::AdapterResult::Unsupported:
        return Error::Unsupported;
      case exchange::AdapterResult::StaleReservation:
        return Error::StaleHandle;
      case exchange::AdapterResult::Failed:
        return Error::NotReady;
    }
    return Error::NotReady;
  }

  exchange::TradeAdapter* Route(
      const ResolvedInstrument& routing) noexcept {
    utils::md::Instrument instrument{};
    instrument.venue = static_cast<utils::md::Venue>(routing.venue);
    instrument.product_type =
        static_cast<utils::md::ProductType>(routing.product_type);
    return router.route(instrument);
  }

  static exchange::AdapterResult OnAdapterEvent(
      void* context, const exchange::AdapterEvent& event) noexcept {
    return static_cast<Impl*>(context)->HandleAdapterEvent(event);
  }

  exchange::AdapterEventSink AdapterSink() noexcept {
    return {this, &Impl::OnAdapterEvent};
  }

  exchange::AdapterResult HandleAdapterEvent(
      const exchange::AdapterEvent& event) noexcept {
    if (event.kind == exchange::AdapterEventKind::Venue) {
      Lane* lane = EventLane(event.venue);
      if (lane == nullptr) {
        if (event.venue.token.sequence != 0)
          return exchange::AdapterResult::WouldBlock;
        unmatched_venue_events.fetch_add(1, std::memory_order_relaxed);
        if (event.venue.type == VenueEventType::Fill)
          unmatched_fills.fetch_add(1, std::memory_order_relaxed);
        return exchange::AdapterResult::Ok;
      }
      if (!HasRoom(*lane, 2)) {
        lane->update_full_count.fetch_add(1, std::memory_order_relaxed);
        return exchange::AdapterResult::WouldBlock;
      }
      OrderRecord* record = EventRecord(event.venue);
      StagedUpdates staged{};
      const Error applied = engine.Apply(event.venue, staged.sink());
      if (applied == Error::NotFound && event.venue.token.sequence != 0)
        return exchange::AdapterResult::WouldBlock;
      if (applied == Error::NotFound) {
        unmatched_venue_events.fetch_add(1, std::memory_order_relaxed);
        if (event.venue.type == VenueEventType::Fill)
          unmatched_fills.fetch_add(1, std::memory_order_relaxed);
      }
      if (applied != Error::Ok) return exchange::AdapterResult::Ok;
      const OrderHandle handle =
          record == nullptr ? OrderHandle{} : orders.HandleOf(*record);
      if (record != nullptr && IsTerminal(record->status) &&
          handle.slot < order_deadlines.size()) {
        const auto deadline = order_deadlines[handle.slot];
        if (deadline.generation != 0 && scheduler.cancel(deadline)) {
          order_deadlines[handle.slot] = {};
          deadline_depth.fetch_sub(1, std::memory_order_relaxed);
        }
      }
      PublishStaged(*lane, staged);
      return exchange::AdapterResult::Ok;
    }
    if (event.kind == exchange::AdapterEventKind::CommandResult) {
      Lane* lane = FindLane(event.command_result.request_token.lane);
      if (lane == nullptr) return exchange::AdapterResult::Ok;
      if (!HasRoom(*lane, 1)) return exchange::AdapterResult::WouldBlock;
      CommandResult(
          *lane,
          event.command_result.kind == exchange::AdapterCommandKind::Place
              ? RuntimeCommandResultKind::Place
              : RuntimeCommandResultKind::Cancel,
          AdapterError(event.command_result.result));
      return exchange::AdapterResult::Ok;
    }
    if (event.kind == exchange::AdapterEventKind::OpenOrderSnapshot ||
        event.kind == exchange::AdapterEventKind::PositionSnapshot ||
        event.kind == exchange::AdapterEventKind::QueryComplete) {
      const api::QueryToken token =
          event.kind == exchange::AdapterEventKind::OpenOrderSnapshot
              ? event.open_order.query_token
              : (event.kind == exchange::AdapterEventKind::PositionSnapshot
                     ? event.position.query_token
                     : event.query_complete.query_token);
      Lane* lane = FindLane(token.lane);
      if (lane == nullptr) return exchange::AdapterResult::Ok;
      if (!HasRoom(*lane, 1)) return exchange::AdapterResult::WouldBlock;
      RuntimeUpdate update{};
      if (event.kind == exchange::AdapterEventKind::OpenOrderSnapshot) {
        update.kind = RuntimeUpdateKind::OpenOrderSnapshot;
        update.open_order = event.open_order;
      } else if (event.kind ==
                 exchange::AdapterEventKind::PositionSnapshot) {
        update.kind = RuntimeUpdateKind::PositionSnapshot;
        update.position = event.position;
      } else {
        update.kind = RuntimeUpdateKind::QueryComplete;
        update.query_complete = event.query_complete;
      }
      Publish(*lane, update);
      return exchange::AdapterResult::Ok;
    }
    if (event.kind == exchange::AdapterEventKind::Status ||
        event.kind == exchange::AdapterEventKind::ReconcileComplete) {
      for (const auto& lane : lanes) {
        if (!HasRoom(*lane, 1)) {
          lane->update_full_count.fetch_add(1, std::memory_order_relaxed);
          return exchange::AdapterResult::WouldBlock;
        }
      }
      RuntimeUpdate update{};
      update.kind = RuntimeUpdateKind::Control;
      update.control.adapter_kind =
          static_cast<std::uint8_t>(event.source.kind);
      if (event.kind == exchange::AdapterEventKind::Status) {
        update.control.kind = RuntimeControlKind::VenueStatus;
        update.control.error = AdapterError(event.status.reason);
        update.control.adapter_status =
            static_cast<std::uint8_t>(event.status.status);
        update.control.event_time_ns = event.status.event_time_ns;
      } else {
        update.control.kind = RuntimeControlKind::ReconcileComplete;
        update.control.error = AdapterError(event.reconcile.result);
        update.control.generation = event.reconcile.generation;
      }
      for (auto& lane : lanes) Publish(*lane, update);
      if (event.kind == exchange::AdapterEventKind::Status) {
        for (std::size_t index = 0; index < adapter_count; ++index) {
          if (adapters[index]->identity().kind == event.source.kind) {
            status_snapshots[index].store(
                static_cast<std::uint8_t>(event.status.status),
                std::memory_order_release);
            break;
          }
        }
      }
    }
    return exchange::AdapterResult::Ok;
  }

  bool IsInlineOwner() const noexcept {
    return config.mode != ExecutionMode::Inline ||
           std::this_thread::get_id() == inline_owner;
  }

  Result<RequestToken> AllocateToken(Lane& lane) noexcept {
    const std::uint32_t epoch =
        lane.session_epoch.load(std::memory_order_acquire);
    if (epoch == 0) return {{}, Error::NotReady};
    const std::uint64_t sequence =
        lane.next_sequence.fetch_add(1, std::memory_order_relaxed) + 1;
    if (sequence == 0) return {{}, Error::CapacityExceeded};
    return {{lane.config.lane_id, epoch, sequence}, Error::Ok};
  }

  Result<QueryToken> SubmitQuery(std::uint32_t lane_id, QueryRequest request,
                                 QueryKind kind) noexcept {
    if (stopping.load(std::memory_order_acquire))
      return {{}, Error::ShuttingDown};
    Lane* lane = FindLane(lane_id);
    if (lane == nullptr || request.account_id == 0 ||
        (request.scope != QueryScope::All &&
         request.scope != QueryScope::SingleInstrument))
      return {{}, Error::InvalidArgument};
    if (!IsInlineOwner()) return {{}, Error::InvalidTransition};
    if (AccountAdapter(request.account_id) == nullptr)
      return {{}, Error::NotFound};
    if (request.scope == QueryScope::SingleInstrument) {
      const InstrumentId instrument_id = directory.Find(request.instrument);
      const auto* entry = directory.Find(instrument_id);
      if (entry == nullptr ||
          entry->lifecycle != ExecutionDirectory::Lifecycle::Active)
        return {{}, Error::NotReady};
    }
    const auto command_kind =
        kind == QueryKind::OpenOrders
            ? runtime::RuntimeCommandKind::QueryOpenOrders
            : runtime::RuntimeCommandKind::QueryPositions;
    if (config.mode == ExecutionMode::DedicatedIo) {
      if (lane->producer_busy.test_and_set(std::memory_order_acquire))
        return {{}, Error::InvalidTransition};
      struct Guard {
        std::atomic_flag& flag;
        ~Guard() { flag.clear(std::memory_order_release); }
      } guard{lane->producer_busy};
      auto lease = lane->commands->try_reserve();
      if (!lease) return {{}, Error::QueueFull};
      const auto token = AllocateToken(*lane);
      if (!token) return token;
      runtime::RuntimeCommand command{};
      command.kind = command_kind;
      command.lane = lane_id;
      command.enqueue_time_ns = NowNs();
      command.query = {token.value, request};
      const bool was_empty = lane->commands->empty();
      lease->emplace(command);
      (void)lease->commit();
      if (was_empty) (void)command_notifier.notify();
      return token;
    }
    if (inline_busy.test_and_set(std::memory_order_acquire))
      return {{}, Error::InvalidTransition};
    const auto token = AllocateToken(*lane);
    if (!token) {
      inline_busy.clear(std::memory_order_release);
      return token;
    }
    runtime::RuntimeCommand command{};
    command.kind = command_kind;
    command.lane = lane_id;
    command.enqueue_time_ns = NowNs();
    command.query = {token.value, request};
    const bool processed = ProcessCommand(*lane, command);
    inline_busy.clear(std::memory_order_release);
    return {token.value, processed ? Error::Ok : Error::QueueFull};
  }

  Result<RequestToken> SubmitInstrumentCommand(
      std::uint32_t lane_id, runtime::RuntimeCommandKind kind,
      InstrumentId instrument_id,
      const ResolvedInstrument* routing = nullptr) noexcept {
    if (stopping.load(std::memory_order_acquire))
      return {{}, Error::ShuttingDown};
    Lane* lane = FindLane(lane_id);
    if (lane == nullptr || instrument_id == 0 ||
        (kind != runtime::RuntimeCommandKind::RegisterInstrument &&
         kind != runtime::RuntimeCommandKind::RetireInstrument) ||
        (kind == runtime::RuntimeCommandKind::RegisterInstrument &&
         routing == nullptr))
      return {{}, Error::InvalidArgument};
    if (!IsInlineOwner()) return {{}, Error::InvalidTransition};

    const auto build = [&](RequestToken token) noexcept {
      runtime::RuntimeCommand command{};
      command.kind = kind;
      command.lane = lane_id;
      command.enqueue_time_ns = NowNs();
      if (kind == runtime::RuntimeCommandKind::RegisterInstrument) {
        command.register_instrument = {token, instrument_id, *routing};
      } else {
        command.retire_instrument = {token, instrument_id};
      }
      return command;
    };

    if (config.mode == ExecutionMode::DedicatedIo) {
      if (lane->producer_busy.test_and_set(std::memory_order_acquire))
        return {{}, Error::InvalidTransition};
      struct Guard {
        std::atomic_flag& flag;
        ~Guard() { flag.clear(std::memory_order_release); }
      } guard{lane->producer_busy};
      auto lease = lane->commands->try_reserve();
      if (!lease) return {{}, Error::QueueFull};
      const auto token = AllocateToken(*lane);
      if (!token) return token;
      const bool was_empty = lane->commands->empty();
      lease->emplace(build(token.value));
      (void)lease->commit();
      if (was_empty) (void)command_notifier.notify();
      return token;
    }

    if (inline_busy.test_and_set(std::memory_order_acquire))
      return {{}, Error::InvalidTransition};
    if (!HasRoom(*lane, 1)) {
      inline_busy.clear(std::memory_order_release);
      return {{}, Error::QueueFull};
    }
    const auto token = AllocateToken(*lane);
    if (!token) {
      inline_busy.clear(std::memory_order_release);
      return token;
    }
    const runtime::RuntimeCommand command = build(token.value);
    const bool processed = ProcessCommand(*lane, command);
    inline_busy.clear(std::memory_order_release);
    return {token.value, processed ? Error::Ok : Error::QueueFull};
  }

  void Publish(Lane& lane, RuntimeUpdate update) noexcept {
    const bool was_empty = lane.updates->empty();
    auto lease = lane.updates->try_reserve();
    if (!lease) {
      lane.update_full_count.fetch_add(1, std::memory_order_relaxed);
      return;
    }
    update.published_time_ns = NowNs();
    lease->emplace(update);
    (void)lease->commit();
    if (was_empty) (void)lane.update_notifier.notify_if_armed();
  }

  void PublishStaged(Lane& lane, StagedUpdates& staged) noexcept {
    for (std::size_t index = 0; index < staged.size; ++index)
      Publish(lane, staged.values[index]);
  }

  bool ValidateToken(Lane& lane, RequestToken token) noexcept {
    const std::uint32_t epoch =
        lane.session_epoch.load(std::memory_order_acquire);
    if (token.lane != lane.config.lane_id || epoch == 0 ||
        token.session_epoch != epoch || token.sequence == 0 ||
        token.sequence != lane.owner_sequence + 1)
      return false;
    lane.owner_sequence = token.sequence;
    return true;
  }

  void CommandResult(Lane& lane, RuntimeCommandResultKind kind, Error error,
                     CancelCommandCorrelation correlation = {}) noexcept {
    RuntimeUpdate update{};
    update.kind = RuntimeUpdateKind::CommandResult;
    update.command_result.kind = kind;
    update.command_result.error = error;
    update.command_result.correlation = correlation;
    Publish(lane, update);
  }

  void InstrumentResult(Lane& lane, RuntimeCommandResultKind kind, Error error,
                        RequestToken request_token,
                        InstrumentId instrument_id) noexcept {
    RuntimeUpdate update{};
    update.kind = RuntimeUpdateKind::CommandResult;
    update.command_result.kind = kind;
    update.command_result.error = error;
    update.command_result.instrument = {request_token, instrument_id};
    Publish(lane, update);
  }

  bool ProcessCommand(Lane& lane,
                      const runtime::RuntimeCommand& command) noexcept {
    // Cancel can emit an order update and its correlated command result. A GTD
    // place can emit Submitted + local Rejected + command error if the
    // deadline scheduler is full. Reserve for the worst case before mutation.
    const bool placing = command.kind == runtime::RuntimeCommandKind::Place;
    const NewOrderRequest* placed_order = &command.place.order;
    const std::size_t required =
        command.kind == runtime::RuntimeCommandKind::RegisterInstrument ||
                command.kind == runtime::RuntimeCommandKind::RetireInstrument
            ? 1
            : (placing &&
                       placed_order->time_in_force == TimeInForce::GTD
                   ? 3
                   : 2);
    if (!HasRoom(lane, required)) {
      lane.update_full_count.fetch_add(1, std::memory_order_relaxed);
      return false;
    }

    if (placing) {
      Error result = Error::InvalidArgument;
      StagedUpdates staged{};
      const auto& submitted = command.place;
      const auto& place = submitted.order;
      if (ValidateToken(lane, place.token)) {
        exchange::TradeAdapter* adapter =
            Route(submitted.routing);
        exchange::AdapterReservation reservation{};
        exchange::AdapterResult reserved =
            adapter == nullptr ? exchange::AdapterResult::NotReady
                               : exchange::preflight_place(
                                     adapter->capabilities(),
                                     place);
        if (reserved == exchange::AdapterResult::Ok) {
          reserved = adapter->reserve_command(
              exchange::AdapterCommandKind::Place, reservation);
        }
        if (reserved != exchange::AdapterResult::Ok) {
          result = AdapterError(reserved);
        } else {
          OrderHandle handle{};
          result = engine.Submit(submitted, handle, staged.sink());
          if (result != Error::Ok) adapter->cancel_reservation(reservation);
          if (result == Error::Ok &&
              place.time_in_force == TimeInForce::GTD) {
            if (scheduler.size() == scheduler.capacity()) {
              deadline_capacity_exceeded.fetch_add(1,
                                                   std::memory_order_relaxed);
              result = Error::DeadlineCapacityExceeded;
              (void)engine.RejectLocal(handle, staged.sink());
              adapter->cancel_reservation(reservation);
            } else {
              const auto scheduled = scheduler.schedule(
                  runtime::DeadlineType::GTD,
                  place.expire_time_ns, handle.slot);
              if (!scheduled) {
                if (scheduled.error ==
                    runtime::DeadlineError::CapacityExceeded) {
                  deadline_capacity_exceeded.fetch_add(
                      1, std::memory_order_relaxed);
                }
                if (scheduled.value.generation != 0)
                  (void)scheduler.cancel(scheduled.value);
                result =
                    scheduled.error == runtime::DeadlineError::CapacityExceeded
                        ? Error::DeadlineCapacityExceeded
                        : Error::NotReady;
                (void)engine.RejectLocal(handle, staged.sink());
                adapter->cancel_reservation(reservation);
              } else {
                gtd_handles[scheduled.value.slot] = handle;
                order_deadlines[handle.slot] = scheduled.value;
                const std::uint64_t depth =
                    deadline_depth.fetch_add(1, std::memory_order_relaxed) + 1;
                auto high =
                    deadline_high_water.load(std::memory_order_relaxed);
                while (depth > high &&
                       !deadline_high_water.compare_exchange_weak(
                           high, depth, std::memory_order_relaxed)) {
                }
              }
            }
          }
          if (result == Error::Ok) {
            exchange::AdapterPlaceCommand adapter_command{};
            adapter_command.command_id =
                (static_cast<std::uint64_t>(lane.config.lane_id) << 32U) ^
                place.token.sequence;
            adapter_command.handle = handle;
            adapter_command.request = place;
            adapter_command.routing = submitted.routing;
            const exchange::AdapterResult committed =
                adapter->commit_place(reservation, adapter_command);
            if (committed != exchange::AdapterResult::Ok) {
              result = AdapterError(committed);
              (void)engine.RejectLocal(handle, staged.sink());
            }
          }
        }
      }
      PublishStaged(lane, staged);
      if (result != Error::Ok)
        CommandResult(lane, RuntimeCommandResultKind::Place, result);
    } else if (command.kind == runtime::RuntimeCommandKind::Cancel) {
      const CancelCommandCorrelation correlation{
          command.cancel.target_token,
          command.cancel.request_token};
      Error result = Error::InvalidArgument;
      StagedUpdates staged{};
      if (ValidateToken(lane, command.cancel.request_token)) {
        OrderRecord* record =
            orders.Find(command.cancel.target_token);
        exchange::TradeAdapter* adapter =
            record == nullptr ? nullptr : Route(record->routing);
        exchange::AdapterReservation reservation{};
        const exchange::AdapterResult reserved =
            adapter == nullptr
                ? exchange::AdapterResult::NotReady
                : adapter->reserve_command(
                      exchange::AdapterCommandKind::Cancel, reservation);
        if (reserved == exchange::AdapterResult::Ok) {
          result =
              engine.RequestCancel(command.cancel.target_token,
                                   staged.sink());
          if (result != Error::Ok) {
            adapter->cancel_reservation(reservation);
          } else {
            exchange::AdapterCancelCommand adapter_command{};
            adapter_command.command_id =
                (static_cast<std::uint64_t>(lane.config.lane_id) << 32U) ^
                command.cancel.request_token.sequence;
            adapter_command.request = command.cancel;
            const exchange::AdapterResult committed =
                adapter->commit_cancel(reservation, adapter_command);
            if (committed != exchange::AdapterResult::Ok)
              result = AdapterError(committed);
          }
        } else {
          result = AdapterError(reserved);
        }
      }
      PublishStaged(lane, staged);
      CommandResult(lane, RuntimeCommandResultKind::Cancel, result,
                    correlation);
    } else if (command.kind == runtime::RuntimeCommandKind::QueryOpenOrders ||
               command.kind == runtime::RuntimeCommandKind::QueryPositions) {
      const auto& request = command.query;
      if (!ValidateToken(lane, request.token)) return true;
      exchange::AdapterResult result = exchange::AdapterResult::Unsupported;
      if (exchange::TradeAdapter* adapter =
              AccountAdapter(request.request.account_id);
          adapter != nullptr) {
        result = command.kind == runtime::RuntimeCommandKind::QueryOpenOrders
                     ? adapter->query_open_orders(request, AdapterSink())
                     : adapter->query_positions(request, AdapterSink());
      } else {
        result = exchange::AdapterResult::InvalidArgument;
      }
      if (result != exchange::AdapterResult::Ok) {
        RuntimeUpdate update{};
        update.kind = RuntimeUpdateKind::QueryComplete;
        update.query_complete.query_token = request.token;
        update.query_complete.account_id = request.request.account_id;
        update.query_complete.kind =
            command.kind == runtime::RuntimeCommandKind::QueryOpenOrders
                ? QueryKind::OpenOrders
                : QueryKind::Positions;
        update.query_complete.error = AdapterError(result);
        Publish(lane, update);
      }
    } else if (command.kind ==
               runtime::RuntimeCommandKind::RegisterInstrument) {
      const auto& request = command.register_instrument;
      Error result = Error::InvalidArgument;
      if (ValidateToken(lane, request.request_token))
        result = directory.Register(request.instrument_id, request.routing);
      InstrumentResult(lane, RuntimeCommandResultKind::RegisterInstrument,
                       result, request.request_token, request.instrument_id);
    } else if (command.kind ==
               runtime::RuntimeCommandKind::RetireInstrument) {
      const auto& request = command.retire_instrument;
      Error result = Error::InvalidArgument;
      if (ValidateToken(lane, request.request_token)) {
        // Only the OMS owner scans OrderTable. StrategyFrame owns and scans
        // its pending-query/position containers before issuing this command.
        result = directory.Retire(
            request.instrument_id,
            orders.HasActiveOrInflight(request.instrument_id));
      }
      InstrumentResult(lane, RuntimeCommandResultKind::RetireInstrument,
                       result, request.request_token, request.instrument_id);
    }
    const std::uint64_t latency = NowNs() - command.enqueue_time_ns;
    lane.enqueue_samples.fetch_add(1, std::memory_order_relaxed);
    lane.enqueue_total_ns.fetch_add(latency, std::memory_order_relaxed);
    auto maximum = lane.enqueue_max_ns.load(std::memory_order_relaxed);
    while (latency > maximum &&
           !lane.enqueue_max_ns.compare_exchange_weak(
               maximum, latency, std::memory_order_relaxed)) {
    }
    return true;
  }

  bool ProcessCommands() noexcept {
    if (config.mode != ExecutionMode::DedicatedIo || lanes.empty()) return false;
    bool progressed = false;
    constexpr std::size_t kBudget = 64;
    for (std::size_t count = 0; count < kBudget; ++count) {
      bool processed = false;
      for (std::size_t scanned = 0; scanned < lanes.size(); ++scanned) {
        const std::size_t index = (round_robin + scanned) % lanes.size();
        auto lease = lanes[index]->commands->try_peek();
        if (!lease) continue;
        if (!ProcessCommand(*lanes[index], **lease)) {
          lanes[index]->starvation_count.fetch_add(1,
                                                   std::memory_order_relaxed);
          lease->cancel();
          continue;
        }
        lease->release();
        round_robin = (index + 1) % lanes.size();
        progressed = true;
        processed = true;
        break;
      }
      if (!processed) break;
    }
    return progressed;
  }

  exchange::TradeAdapter* AccountAdapter(AccountId account_id) noexcept {
    const auto found = std::lower_bound(
        account_routes.begin(), account_routes.end(), account_id,
        [](const AdapterRuntimeConfig::AccountRoute& route, AccountId id) {
          return route.account_id < id;
        });
    return found == account_routes.end() || found->account_id != account_id
               ? nullptr
               : router.find(found->adapter);
  }

  bool ProcessDeadlines() noexcept {
    bool progressed = false;
    const std::uint64_t now = NowNs();
    for (;;) {
      const auto earliest = scheduler.earliest_deadline();
      if (!earliest || *earliest > now) break;
      const auto due = scheduler.pop_due(now);
      if (!due || !due.has_value) break;
      deadline_depth.fetch_sub(1, std::memory_order_relaxed);
      const OrderHandle handle = gtd_handles[due.value.handle.slot];
      if (handle.slot < order_deadlines.size())
        order_deadlines[handle.slot] = {};
      OrderRecord* record = orders.Lookup(handle);
      if (record == nullptr) continue;
      Lane* lane = FindLane(record->request.token.lane);
      if (lane == nullptr || !HasRoom(*lane, 2)) {
        const auto retry = scheduler.schedule(runtime::DeadlineType::GTD, now,
                                              handle.slot);
        if (retry.value.generation != 0) {
          gtd_handles[retry.value.slot] = handle;
          order_deadlines[handle.slot] = retry.value;
          deadline_depth.fetch_add(1, std::memory_order_relaxed);
        }
        return progressed;
      }
      VenueEvent event{};
      event.type = VenueEventType::Expire;
      event.handle = handle;
      StagedUpdates staged{};
      (void)engine.Apply(event, staged.sink());
      PublishStaged(*lane, staged);
      progressed = true;
    }
    return progressed;
  }

  bool ServiceAdapters() noexcept {
    bool progressed = false;
    const std::uint64_t now = NowNs();
    for (std::size_t index = 0; index < adapter_count; ++index) {
      exchange::TradeAdapter& adapter = *adapters[index];
      if (adapter_deadlines[index] != 0 &&
          adapter_deadlines[index] <= now) {
        exchange::AdapterDeadline deadline{};
        deadline.kind =
            adapter.identity().kind == exchange::AdapterKind::Polymarket
                ? exchange::AdapterDeadlineKind::Request
                : exchange::AdapterDeadlineKind::Keepalive;
        deadline.due_time_ns = adapter_deadlines[index];
        const auto result = adapter.on_deadline(deadline, now, AdapterSink());
        if (result != exchange::AdapterResult::WouldBlock)
          adapter_deadlines[index] = 0;
        progressed = result == exchange::AdapterResult::Ok || progressed;
      }
      if (adapter.status() == exchange::AdapterStatus::Reconnecting &&
          reconnect_deadlines[index] <= now) {
        exchange::AdapterDeadline deadline{};
        deadline.kind = exchange::AdapterDeadlineKind::Reconnect;
        deadline.due_time_ns = now;
        const auto result = adapter.on_deadline(deadline, now, AdapterSink());
        reconnect_deadlines[index] = now + 1'000'000'000ULL;
        progressed = result == exchange::AdapterResult::Ok || progressed;
      }
      exchange::AdapterServiceResult serviced{};
      if (&adapter == &fake) {
        std::lock_guard lock(fake_mutex);
        serviced = adapter.service_io(now, adapter_event_budget, AdapterSink());
      } else {
        serviced = adapter.service_io(now, adapter_event_budget, AdapterSink());
      }
      if (serviced.next_deadline_ns != 0)
        adapter_deadlines[index] = serviced.next_deadline_ns;
      const auto current_status = adapter.status();
      const auto published_status = static_cast<exchange::AdapterStatus>(
          status_snapshots[index].load(std::memory_order_acquire));
      if (current_status != published_status) {
        exchange::AdapterEvent status_event{};
        status_event.kind = exchange::AdapterEventKind::Status;
        status_event.source = adapter.identity();
        status_event.status.identity = status_event.source;
        status_event.status.status = current_status;
        status_event.status.reason = serviced.result;
        status_event.status.event_time_ns = now;
        (void)HandleAdapterEvent(status_event);
      }
      progressed = serviced.events_processed != 0 || progressed;
    }
    return progressed;
  }

  std::uint64_t NextIoToken() noexcept {
    do {
      ++next_io_token;
    } while (next_io_token <= kTimerToken);
    return next_io_token;
  }

  void SyncIoDescriptors() noexcept {
    for (auto& registration : io_registrations) registration.seen = false;
    std::size_t offset = 0;
    for (std::size_t driver_index = 0;
         driver_index < io_drivers.size(); ++driver_index) {
      exchange::AsyncIoDriver* driver = io_drivers[driver_index];
      const std::size_t capacity = io_driver_capacities[driver_index];
      if (capacity > io_descriptors.size() - offset) break;
      const auto output =
          std::span<exchange::AsyncIoDescriptor>(io_descriptors)
              .subspan(offset, capacity);
      const std::size_t count =
          std::min(driver->snapshot_descriptors(output), output.size());
      for (std::size_t index = 0; index < count; ++index) {
        const exchange::AsyncIoDescriptor descriptor = output[index];
        if (descriptor.fd < 0 || descriptor.events == 0 ||
            descriptor.generation == 0) {
          continue;
        }
        auto current = std::find_if(
            io_registrations.begin(), io_registrations.end(),
            [driver, descriptor](const IoRegistration& value) noexcept {
              return value.driver == driver && value.fd == descriptor.fd;
            });
        if (current != io_registrations.end()) {
          current->seen = true;
          if (current->generation != descriptor.generation) {
            (void)::epoll_ctl(epoll_fd, EPOLL_CTL_DEL, current->fd, nullptr);
            current->generation = descriptor.generation;
            current->events = descriptor.events;
            current->token = NextIoToken();
            epoll_event event{};
            event.events = descriptor.events;
            event.data.u64 = current->token;
            if (::epoll_ctl(epoll_fd, EPOLL_CTL_ADD, descriptor.fd, &event) !=
                0) {
              current->seen = false;
            }
          } else if (current->events != descriptor.events) {
            epoll_event event{};
            event.events = descriptor.events;
            event.data.u64 = current->token;
            if (::epoll_ctl(epoll_fd, EPOLL_CTL_MOD, descriptor.fd, &event) ==
                0) {
              current->events = descriptor.events;
            }
          }
          continue;
        }
        const bool fd_in_use = std::any_of(
            io_registrations.begin(), io_registrations.end(),
            [descriptor](const IoRegistration& value) noexcept {
              return value.fd == descriptor.fd;
            });
        if (fd_in_use) continue;
        IoRegistration registration{driver, descriptor.fd, descriptor.events,
                                    descriptor.generation, NextIoToken(), true};
        epoll_event event{};
        event.events = descriptor.events;
        event.data.u64 = registration.token;
        if (::epoll_ctl(epoll_fd, EPOLL_CTL_ADD, descriptor.fd, &event) == 0) {
          io_registrations.push_back(registration);
        }
      }
      offset += capacity;
    }
    for (std::size_t index = io_registrations.size(); index != 0; --index) {
      if (io_registrations[index - 1].seen) continue;
      (void)::epoll_ctl(epoll_fd, EPOLL_CTL_DEL,
                        io_registrations[index - 1].fd, nullptr);
      io_registrations.erase(io_registrations.begin() +
                             static_cast<std::ptrdiff_t>(index - 1));
    }
  }

  bool CheckIoTimeouts(std::uint64_t now) noexcept {
    bool due = false;
    for (exchange::AsyncIoDriver* driver : io_drivers) {
      const std::uint64_t deadline = driver->next_deadline_ns();
      due = (deadline != 0 && deadline <= now) || due;
      driver->check_timeouts(now);
    }
    return due;
  }

  int AdapterWait(int requested) noexcept {
    std::uint64_t due = 0;
    const auto consider = [&due](std::uint64_t candidate) {
      if (candidate != 0 && (due == 0 || candidate < due)) due = candidate;
    };
    {
      std::lock_guard lock(fake_mutex);
      due = fake.next_event_ns();
    }
    for (std::size_t index = 0; index < adapter_count; ++index) {
      consider(adapter_deadlines[index]);
      if (adapters[index]->status() == exchange::AdapterStatus::Reconnecting)
        consider(reconnect_deadlines[index]);
    }
    for (exchange::AsyncIoDriver* driver : io_drivers)
      consider(driver->next_deadline_ns());
    return due == 0 ? requested
                    : ToWaitMilliseconds(due, NowNs(), requested);
  }

  void Wait(int timeout_ms) noexcept {
    SyncIoDescriptors();
    int result;
    do {
      result = ::epoll_wait(epoll_fd, epoll_events.data(),
                            static_cast<int>(epoll_events.size()),
                            AdapterWait(timeout_ms));
    } while (result < 0 && errno == EINTR);
    if (result <= 0) return;
    // Re-snapshot after wakeup so events queued for a descriptor generation
    // replaced while epoll_wait was blocked cannot reach the driver.
    SyncIoDescriptors();
    for (int index = 0; index < result; ++index) {
      const auto event_index = static_cast<std::size_t>(index);
      const std::uint64_t token = epoll_events[event_index].data.u64;
      if (token == kCommandToken) {
        (void)command_notifier.drain();
      } else if (token == kTimerToken) {
        (void)timer.drain();
      } else {
        const auto registration = std::find_if(
            io_registrations.begin(), io_registrations.end(),
            [token](const IoRegistration& value) noexcept {
              return value.token == token;
            });
        if (registration != io_registrations.end()) {
          registration->driver->service_io(
              registration->fd, registration->generation,
              epoll_events[event_index].events, NowNs());
        }
      }
    }
  }

  bool Work() noexcept {
    bool progressed = ProcessCommands();
    progressed = ProcessDeadlines() || progressed;
    progressed = CheckIoTimeouts(NowNs()) || progressed;
    const std::uint32_t requested =
        reconcile_mask.exchange(0, std::memory_order_acq_rel);
    if (requested != 0) {
      for (std::size_t index = 0; index < adapter_count; ++index) {
        const auto kind = adapters[index]->identity().kind;
        const std::uint32_t bit =
            1U << static_cast<std::uint8_t>(kind);
        if ((requested & bit) == 0) continue;
        std::uint64_t generation =
            next_reconcile_generation.fetch_add(1, std::memory_order_relaxed);
        if (generation == 0)
          generation =
              next_reconcile_generation.fetch_add(1, std::memory_order_relaxed);
        (void)adapters[index]->begin_reconcile(generation, NowNs(),
                                               AdapterSink());
        progressed = true;
      }
    }
    progressed = ServiceAdapters() || progressed;
    return progressed;
  }

  void Run(std::stop_token token) noexcept {
    while (!token.stop_requested() &&
           !stopping.load(std::memory_order_acquire)) {
      if (!Work()) Wait(-1);
    }
  }

  Error Service(int timeout_ms) noexcept {
    if (config.mode != ExecutionMode::Inline) return Error::InvalidMode;
    if (timeout_ms < -1) return Error::InvalidArgument;
    if (!IsInlineOwner()) return Error::InvalidTransition;
    if (stopping.load(std::memory_order_acquire)) return Error::ShuttingDown;
    if (inline_busy.test_and_set(std::memory_order_acquire))
      return Error::InvalidTransition;
    struct Guard {
      std::atomic_flag& flag;
      ~Guard() { flag.clear(std::memory_order_release); }
    } guard{inline_busy};
    if (!Work()) Wait(timeout_ms);
    (void)Work();
    return Error::Ok;
  }

  Error Stop() noexcept {
    if (stopping.exchange(true, std::memory_order_acq_rel)) return Error::Ok;
    if (worker.joinable()) {
      worker.request_stop();
      (void)command_notifier.notify();
      worker.join();
    }
    for (std::size_t index = 0; index < adapter_count; ++index)
      (void)adapters[index]->shutdown(NowNs(), AdapterSink());
    return Error::Ok;
  }

  RuntimeConfig config{};
  std::uint32_t adapter_event_budget{};
  std::thread::id inline_owner{};
  ExecutionDirectory directory;
  OrderTable orders;
  StateEngine engine;
  runtime::EventNotifier command_notifier;
  runtime::TimerNotifier timer;
  runtime::DeadlineScheduler scheduler;
  exchange::AdapterRouter router;
  exchange::FakeTradeAdapter fake;
  std::array<exchange::TradeAdapter*, exchange::kMaxTradeAdapters> adapters{};
  std::array<std::uint64_t, exchange::kMaxTradeAdapters> adapter_deadlines{};
  std::array<std::uint64_t, exchange::kMaxTradeAdapters> reconnect_deadlines{};
  std::array<std::atomic<std::uint8_t>, exchange::kMaxTradeAdapters>
      status_snapshots{};
  std::size_t adapter_count{};
  std::vector<exchange::AsyncIoDriver*> io_drivers;
  std::vector<AdapterRuntimeConfig::AccountRoute> account_routes;
  std::vector<std::size_t> io_driver_capacities;
  std::vector<exchange::AsyncIoDescriptor> io_descriptors;
  std::vector<IoRegistration> io_registrations;
  std::vector<epoll_event> epoll_events;
  std::uint64_t next_io_token{kTimerToken};
  std::atomic<std::uint32_t> reconcile_mask{0};
  std::atomic<std::uint64_t> next_reconcile_generation{1};
  std::vector<OrderHandle> gtd_handles;
  std::vector<runtime::DeadlineHandle> order_deadlines;
  std::vector<std::unique_ptr<Lane>> lanes;
  std::mutex fake_mutex;
  std::size_t round_robin{};
  std::atomic<std::uint64_t> deadline_depth{0};
  std::atomic<std::uint64_t> deadline_high_water{0};
  std::atomic<std::uint64_t> deadline_capacity_exceeded{0};
  std::atomic<std::uint64_t> unmatched_venue_events{0};
  std::atomic<std::uint64_t> unmatched_fills{0};
  int epoll_fd{-1};
  std::atomic<bool> stopping{false};
  std::atomic_flag inline_busy = ATOMIC_FLAG_INIT;
  std::jthread worker;
};

OmsApi::OmsApi(std::unique_ptr<Impl> impl) noexcept : impl_(std::move(impl)) {}
OmsApi::~OmsApi() = default;

Result<std::unique_ptr<OmsApi>> OmsApi::Create(
    const RuntimeConfig& config, std::span<const InstrumentInit> instruments,
    std::span<const ReplayStep> replay) {
  return Create(config, instruments, replay, AdapterRuntimeConfig{});
}

Result<std::unique_ptr<OmsApi>> OmsApi::Create(
    const RuntimeConfig& config, std::span<const InstrumentInit> instruments,
    std::span<const ReplayStep> replay,
    const AdapterRuntimeConfig& adapter_config) {
  if (config.mode != ExecutionMode::Inline &&
      config.mode != ExecutionMode::DedicatedIo)
    return {{}, Error::InvalidMode};
  const RuntimeConfig normalized = NormalizeRuntimeConfig(config);
  if (!ValidateRuntimeCapacities(normalized) ||
      normalized.lane_count == 0 ||
      normalized.lane_count > kMaxRuntimeLanes ||
      normalized.deadline_capacity == 0)
    return {{}, Error::InvalidArgument};
  for (std::uint32_t index = 0; index < normalized.lane_count; ++index) {
    const LaneConfig& lane = normalized.lanes[index];
    if (!ValidCapacity(lane.update_capacity) ||
        (normalized.mode == ExecutionMode::DedicatedIo &&
         !ValidCapacity(lane.command_capacity)))
      return {{}, Error::InvalidArgument};
    for (std::uint32_t previous = 0; previous < index; ++previous)
      if (normalized.lanes[previous].lane_id == lane.lane_id)
        return {{}, Error::InvalidArgument};
  }
  try {
    auto impl =
        std::make_unique<Impl>(normalized, instruments, replay, adapter_config);
    return {std::unique_ptr<OmsApi>(new OmsApi(std::move(impl))), Error::Ok};
  } catch (const std::invalid_argument&) {
    return {{}, Error::InvalidArgument};
  } catch (...) {
    return {{}, Error::NotReady};
  }
}

Result<void> OmsApi::initialize_lane(std::uint32_t lane_id,
                                     std::uint32_t epoch) noexcept {
  if (!impl_->IsInlineOwner()) return {Error::InvalidTransition};
  auto* lane = impl_->FindLane(lane_id);
  if (lane == nullptr || epoch == 0) return {Error::InvalidArgument};
  std::uint32_t expected = 0;
  if (!lane->session_epoch.compare_exchange_strong(expected, epoch))
    return {Error::Conflict};
  return {};
}

Result<RequestToken> OmsApi::submit_order(std::uint32_t lane_id,
                                          SubmitOrderRequest request) noexcept {
  return submit_order_impl(lane_id, request);
}

Result<RequestToken> OmsApi::submit_order_impl(
    std::uint32_t lane_id,
    SubmitOrderRequest& request) noexcept {
  if (impl_->stopping.load(std::memory_order_acquire))
    return {{}, Error::ShuttingDown};
  auto* lane = impl_->FindLane(lane_id);
  if (lane == nullptr) return {{}, Error::InvalidArgument};
  if (!impl_->IsInlineOwner()) return {{}, Error::InvalidTransition};
  if (request.order.instrument_id == 0)
    return {{}, Error::InvalidArgument};
  if (impl_->config.mode == ExecutionMode::DedicatedIo) {
    if (request.order.time_in_force == TimeInForce::GTD &&
        lane->updates->capacity() < 3)
      return {{}, Error::QueueFull};
    if (lane->producer_busy.test_and_set(std::memory_order_acquire))
      return {{}, Error::InvalidTransition};
    struct ProducerGuard {
      std::atomic_flag& flag;
      ~ProducerGuard() { flag.clear(std::memory_order_release); }
    } producer_guard{lane->producer_busy};
    auto lease = lane->commands->try_reserve();
    if (!lease) {
      lane->command_full_count.fetch_add(1, std::memory_order_relaxed);
      return {{}, Error::QueueFull};
    }
    const auto token = impl_->AllocateToken(*lane);
    if (!token) return token;
    request.order.token = token.value;
    auto& command = lease->emplace();
    command.kind = runtime::RuntimeCommandKind::Place;
    command.lane = lane_id;
    command.enqueue_time_ns = NowNs();
    command.place = request;
    const bool was_empty = lane->commands->empty();
    (void)lease->commit();
    if (was_empty) (void)impl_->command_notifier.notify();
    return token;
  }
  if (impl_->inline_busy.test_and_set(std::memory_order_acquire))
    return {{}, Error::InvalidTransition};
  const std::size_t required =
      request.order.time_in_force == TimeInForce::GTD ? 3 : 2;
  if (!impl_->HasRoom(*lane, required)) {
    impl_->inline_busy.clear(std::memory_order_release);
    lane->update_full_count.fetch_add(1, std::memory_order_relaxed);
    return {{}, Error::QueueFull};
  }
  const auto token = impl_->AllocateToken(*lane);
  if (!token) {
    impl_->inline_busy.clear(std::memory_order_release);
    return token;
  }
  request.order.token = token.value;
  runtime::RuntimeCommand command{};
  command.kind = runtime::RuntimeCommandKind::Place;
  command.lane = lane_id;
  command.enqueue_time_ns = NowNs();
  command.place = request;
  const bool processed = impl_->ProcessCommand(*lane, command);
  impl_->inline_busy.clear(std::memory_order_release);
  return {token.value, processed ? Error::Ok : Error::QueueFull};
}

Result<RequestToken> OmsApi::cancel_order(std::uint32_t lane_id,
                                          RequestToken target,
                                          OrderHandle handle) noexcept {
  if (impl_->stopping.load(std::memory_order_acquire))
    return {{}, Error::ShuttingDown};
  auto* lane = impl_->FindLane(lane_id);
  if (lane == nullptr) return {{}, Error::InvalidArgument};
  if (!impl_->IsInlineOwner()) return {{}, Error::InvalidTransition};
  if (impl_->config.mode == ExecutionMode::DedicatedIo) {
    if (lane->producer_busy.test_and_set(std::memory_order_acquire))
      return {{}, Error::InvalidTransition};
    struct ProducerGuard {
      std::atomic_flag& flag;
      ~ProducerGuard() { flag.clear(std::memory_order_release); }
    } producer_guard{lane->producer_busy};
    auto lease = lane->commands->try_reserve();
    if (!lease) {
      lane->command_full_count.fetch_add(1, std::memory_order_relaxed);
      return {{}, Error::QueueFull};
    }
    const auto token = impl_->AllocateToken(*lane);
    if (!token) return token;
    auto& command = lease->emplace();
    command.kind = runtime::RuntimeCommandKind::Cancel;
    command.lane = lane_id;
    command.enqueue_time_ns = NowNs();
    command.cancel = {token.value, target, handle};
    const bool was_empty = lane->commands->empty();
    (void)lease->commit();
    if (was_empty) (void)impl_->command_notifier.notify();
    return token;
  }
  if (impl_->inline_busy.test_and_set(std::memory_order_acquire))
    return {{}, Error::InvalidTransition};
  if (!impl_->HasRoom(*lane, 2)) {
    impl_->inline_busy.clear(std::memory_order_release);
    lane->update_full_count.fetch_add(1, std::memory_order_relaxed);
    return {{}, Error::QueueFull};
  }
  const auto token = impl_->AllocateToken(*lane);
  if (!token) {
    impl_->inline_busy.clear(std::memory_order_release);
    return token;
  }
  runtime::RuntimeCommand command{};
  command.kind = runtime::RuntimeCommandKind::Cancel;
  command.lane = lane_id;
  command.enqueue_time_ns = NowNs();
  command.cancel = {token.value, target, handle};
  const bool processed = impl_->ProcessCommand(*lane, command);
  impl_->inline_busy.clear(std::memory_order_release);
  return {token.value, processed ? Error::Ok : Error::QueueFull};
}

Result<RequestToken> OmsApi::register_instrument(
    std::uint32_t lane_id, RegisterInstrumentRequest request) noexcept {
  return impl_->SubmitInstrumentCommand(
      lane_id, runtime::RuntimeCommandKind::RegisterInstrument,
      request.instrument_id, &request.routing);
}

Result<RequestToken> OmsApi::retire_instrument(
    std::uint32_t lane_id, InstrumentId instrument_id) noexcept {
  return impl_->SubmitInstrumentCommand(
      lane_id, runtime::RuntimeCommandKind::RetireInstrument, instrument_id);
}

Result<QueryToken> OmsApi::query_open_orders(std::uint32_t lane_id,
                                             AccountId account_id) noexcept {
  QueryRequest request{};
  request.account_id = account_id;
  return query_open_orders(lane_id, request);
}

Result<QueryToken> OmsApi::query_open_orders(std::uint32_t lane_id,
                                             QueryRequest request) noexcept {
  return impl_->SubmitQuery(lane_id, request, QueryKind::OpenOrders);
}

Result<QueryToken> OmsApi::query_positions(std::uint32_t lane_id,
                                           AccountId account_id) noexcept {
  QueryRequest request{};
  request.account_id = account_id;
  return query_positions(lane_id, request);
}

Result<QueryToken> OmsApi::query_positions(std::uint32_t lane_id,
                                           QueryRequest request) noexcept {
  return impl_->SubmitQuery(lane_id, request, QueryKind::Positions);
}

Error OmsApi::service_io(int timeout_ms) noexcept {
  return impl_->Service(timeout_ms);
}

std::size_t OmsApi::drain_updates(std::uint32_t lane_id,
                                  UpdateCallback callback, void* context,
                                  std::size_t maximum) noexcept {
  auto* lane = impl_->FindLane(lane_id);
  if (lane == nullptr || callback == nullptr) return 0;
  std::size_t count = 0;
  for (;;) {
    while (count < maximum) {
      auto lease = lane->updates->try_peek();
      if (!lease) break;
      const RuntimeUpdate update = **lease;
      lease->release();
      const std::uint64_t now = NowNs();
      const std::uint64_t latency =
          now >= update.published_time_ns ? now - update.published_time_ns : 0;
      lane->drain_samples.fetch_add(1, std::memory_order_relaxed);
      lane->drain_total_ns.fetch_add(latency, std::memory_order_relaxed);
      lane->drain_latest_ns.store(latency, std::memory_order_relaxed);
      auto maximum_seen = lane->drain_max_ns.load(std::memory_order_relaxed);
      while (latency > maximum_seen &&
             !lane->drain_max_ns.compare_exchange_weak(
                 maximum_seen, latency, std::memory_order_relaxed)) {
      }
      callback(context, update);
      ++count;
    }
    (void)lane->update_notifier.drain();
    lane->update_notifier.arm();
    const bool empty = lane->updates->empty();
    if (!empty) (void)lane->update_notifier.notify_if_armed();
    if (empty || count == maximum) break;
  }
  if (impl_->config.mode == ExecutionMode::DedicatedIo && count != 0)
    (void)impl_->command_notifier.notify();
  return count;
}

int OmsApi::update_fd(std::uint32_t lane_id) const noexcept {
  if (impl_->config.mode == ExecutionMode::Inline) return -1;
  const auto* lane = impl_->FindLane(lane_id);
  return lane == nullptr ? -1 : lane->update_notifier.fd();
}

RuntimeMetrics OmsApi::metrics() const noexcept {
  RuntimeMetrics result{};
  result.mode = impl_->config.mode;
  result.lane_count = impl_->config.lane_count;
  result.deadlines.capacity = impl_->scheduler.capacity();
  result.deadlines.depth =
      impl_->deadline_depth.load(std::memory_order_relaxed);
  result.deadlines.high_water =
      impl_->deadline_high_water.load(std::memory_order_relaxed);
  result.deadlines.capacity_exceeded_count =
      impl_->deadline_capacity_exceeded.load(std::memory_order_relaxed);
  result.unmatched_venue_events =
      impl_->unmatched_venue_events.load(std::memory_order_relaxed);
  result.unmatched_fills =
      impl_->unmatched_fills.load(std::memory_order_relaxed);
  for (std::size_t index = 0; index < impl_->lanes.size(); ++index) {
    const auto& source = *impl_->lanes[index];
    auto& target = result.lanes[index];
    target.lane_id = source.config.lane_id;
    if (source.commands) {
      target.command_queue.capacity = source.commands->capacity();
      target.command_queue.depth = source.commands->depth();
      target.command_queue.high_water = source.commands->high_water();
    }
    target.command_queue.full_count =
        source.command_full_count.load(std::memory_order_relaxed);
    target.update_queue.capacity = source.updates->capacity();
    target.update_queue.depth = source.updates->depth();
    target.update_queue.high_water = source.updates->high_water();
    target.update_queue.full_count =
        source.update_full_count.load(std::memory_order_relaxed);
    target.enqueue_to_owner.sample_count =
        source.enqueue_samples.load(std::memory_order_relaxed);
    target.enqueue_to_owner.total_ns =
        source.enqueue_total_ns.load(std::memory_order_relaxed);
    target.enqueue_to_owner.maximum_ns =
        source.enqueue_max_ns.load(std::memory_order_relaxed);
    target.owner_to_drain.sample_count =
        source.drain_samples.load(std::memory_order_relaxed);
    target.owner_to_drain.total_ns =
        source.drain_total_ns.load(std::memory_order_relaxed);
    target.owner_to_drain.maximum_ns =
        source.drain_max_ns.load(std::memory_order_relaxed);
    target.owner_to_drain.latest_ns =
        source.drain_latest_ns.load(std::memory_order_relaxed);
    target.starvation_count =
        source.starvation_count.load(std::memory_order_relaxed);
  }
  return result;
}

Result<AdapterStatusSnapshot> OmsApi::adapter_status(
    exchange::AdapterKind kind) const noexcept {
  for (std::size_t index = 0; index < impl_->adapter_count; ++index) {
    const auto identity = impl_->adapters[index]->identity();
    if (identity.kind != kind) continue;
    AdapterStatusSnapshot snapshot{};
    snapshot.identity = identity;
    snapshot.status = static_cast<exchange::AdapterStatus>(
        impl_->status_snapshots[index].load(std::memory_order_acquire));
    snapshot.capabilities = impl_->adapters[index]->capabilities();
    return {snapshot, Error::Ok};
  }
  return {{}, Error::NotFound};
}

InstrumentId OmsApi::resolve_polymarket_token(
    const std::array<std::uint8_t, 32>& token_id) const noexcept {
  return impl_->directory.FindPolymarketToken(token_id);
}

std::uint64_t OmsApi::execution_directory_access_count() const noexcept {
  return impl_->directory.access_count();
}

Error OmsApi::reconcile(exchange::AdapterKind kind) noexcept {
  if (impl_->stopping.load(std::memory_order_acquire))
    return Error::ShuttingDown;
  if (impl_->router.find(kind) == nullptr) return Error::NotFound;
  const std::uint8_t value = static_cast<std::uint8_t>(kind);
  if (value >= 31) return Error::InvalidArgument;
  impl_->reconcile_mask.fetch_or(1U << value, std::memory_order_release);
  (void)impl_->command_notifier.notify();
  return Error::Ok;
}

Error OmsApi::shutdown() noexcept { return impl_->Stop(); }

Error OmsApi::inject(const ReplayStep& step) noexcept {
  if (impl_->stopping.load(std::memory_order_acquire))
    return Error::ShuttingDown;
  std::lock_guard lock(impl_->fake_mutex);
  const auto control =
      step.control == ReplayControl::Disconnect
          ? exchange::FakeTradeAdapter::Control::Disconnect
          : (step.control == ReplayControl::Reconnect
                 ? exchange::FakeTradeAdapter::Control::Reconnect
                 : exchange::FakeTradeAdapter::Control::Event);
  const std::uint64_t previous = impl_->fake.last_event_ns();
  const std::uint64_t base = previous == 0 ? NowNs() : previous;
  const exchange::AdapterResult injected =
      impl_->fake.inject(control, step.event, base + step.delay_ns);
  if (injected != exchange::AdapterResult::Ok)
    return injected == exchange::AdapterResult::WouldBlock
               ? Error::CapacityExceeded
               : Error::NotReady;
  (void)impl_->command_notifier.notify();
  return Error::Ok;
}

}  // namespace oms::api

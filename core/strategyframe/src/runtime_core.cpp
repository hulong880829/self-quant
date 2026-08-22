#include "strategyframe/detail/runtime_bridge.h"

#include <algorithm>
#include <atomic>
#include <chrono>
#include <cstdlib>
#include <cstring>
#include <iterator>
#include <limits>
#include <memory>
#include <stdexcept>
#include <string>
#include <thread>
#include <utility>
#include <vector>

#include <pthread.h>
#include <sched.h>
#include <linux/mempolicy.h>
#include <poll.h>
#include <sys/mman.h>
#include <sys/syscall.h>
#include <sys/timerfd.h>
#include <unistd.h>
#if defined(__x86_64__) || defined(__i386__)
#include <x86intrin.h>
#endif

#include "oms/api/execution_channel.h"
#include "strategyframe/market_data_source.h"

namespace strategyframe {
namespace {

constexpr std::uint32_t kLane = 1;
static_assert(static_cast<std::uint16_t>(OrderFlag::PostOnly) ==
              static_cast<std::uint16_t>(oms::api::OrderFlag::PostOnly));
static_assert(static_cast<std::uint16_t>(OrderFlag::ReduceOnly) ==
              static_cast<std::uint16_t>(oms::api::OrderFlag::ReduceOnly));
static_assert(static_cast<std::uint16_t>(OrderFlag::ClosePosition) ==
              static_cast<std::uint16_t>(
                  oms::api::OrderFlag::ClosePosition));
static_assert(static_cast<std::uint16_t>(OrderFlag::QuoteQuantity) ==
              static_cast<std::uint16_t>(
                  oms::api::OrderFlag::QuoteQuantity));
std::uint64_t MonotonicNowNs() noexcept {
  return static_cast<std::uint64_t>(
      std::chrono::duration_cast<std::chrono::nanoseconds>(
          std::chrono::steady_clock::now().time_since_epoch())
          .count());
}

std::uint64_t WallNowNs() noexcept {
  return static_cast<std::uint64_t>(
      std::chrono::duration_cast<std::chrono::nanoseconds>(
          std::chrono::system_clock::now().time_since_epoch())
          .count());
}

std::uint64_t ReadTsc() noexcept {
#if defined(__x86_64__) || defined(__i386__)
  return __rdtsc();
#else
  return 0;
#endif
}

Error FromOms(oms::api::Error error) noexcept {
  using O = oms::api::Error;
  switch (error) {
    case O::Ok:
      return Error::Ok;
    case O::InvalidArgument:
    case O::InvalidScale:
      return Error::InvalidArgument;
    case O::NotFound:
    case O::StaleHandle:
      return Error::NotFound;
    case O::CapacityExceeded:
    case O::DeadlineCapacityExceeded:
      return Error::CapacityExceeded;
    case O::QueueFull:
      return Error::WouldBlock;
    case O::InvalidTransition:
    case O::Conflict:
      return Error::InvalidState;
    case O::NotReady:
      return Error::NotReady;
    case O::ShuttingDown:
      return Error::ShuttingDown;
    case O::Unsupported:
      return Error::Unsupported;
    default:
      return Error::OmsFailure;
  }
}

OrderToken FromOms(oms::api::RequestToken value) noexcept {
  return {value.lane, value.session_epoch, value.sequence};
}

oms::api::RequestToken ToOms(OrderToken value) noexcept {
  return {value.lane, value.session_epoch, value.sequence};
}

FixedPoint FromOms(oms::api::FixedPoint value) noexcept {
  return {value.value, value.scale, {}};
}

template <typename Output, typename Input>
Output CopyId(const Input& value) noexcept {
  Output result{};
  const std::size_t size = std::min<std::size_t>(
      value.length, sizeof(result.value));
  if (size != 0) std::memcpy(result.value, value.value.data(), size);
  result.length = static_cast<std::uint16_t>(size);
  return result;
}

oms::api::FixedPoint ToOms(FixedPoint value) noexcept {
  return {value.value, value.scale, {}};
}

OrderStatus FromOms(oms::api::OrderStatus value) noexcept {
  return static_cast<OrderStatus>(value);
}

const char* Environment(std::string_view name) noexcept {
  if (name.empty()) return nullptr;
  const std::string owned(name);
  return std::getenv(owned.c_str());
}

int HexNibble(char value) noexcept {
  if (value >= '0' && value <= '9') return value - '0';
  if (value >= 'a' && value <= 'f') return value - 'a' + 10;
  if (value >= 'A' && value <= 'F') return value - 'A' + 10;
  return -1;
}

bool ParseUint256(std::string_view text,
                  std::array<std::uint8_t, 32>& output) noexcept {
  output.fill(0);
  if (text.starts_with("0x") || text.starts_with("0X")) {
    text.remove_prefix(2);
    if (text.size() != output.size() * 2) return false;
    for (std::size_t index = 0; index < output.size(); ++index) {
      const int high = HexNibble(text[index * 2]);
      const int low = HexNibble(text[index * 2 + 1]);
      if (high < 0 || low < 0) return false;
      output[index] =
          static_cast<std::uint8_t>((high << 4) | low);
    }
    return true;
  }
  if (text.empty()) return false;
  for (const char value : text) {
    if (value < '0' || value > '9') return false;
    unsigned carry = static_cast<unsigned>(value - '0');
    for (std::size_t index = output.size(); index != 0; --index) {
      const unsigned current =
          static_cast<unsigned>(output[index - 1]) * 10U + carry;
      output[index - 1] = static_cast<std::uint8_t>(current & 0xffU);
      carry = current >> 8U;
    }
    if (carry != 0) return false;
  }
  return true;
}

bool BindCurrentThread(std::int32_t cpu) noexcept {
  if (cpu < 0) return true;
  if (cpu >= CPU_SETSIZE) return false;
  cpu_set_t set;
  CPU_ZERO(&set);
  CPU_SET(static_cast<unsigned>(cpu), &set);
  return pthread_setaffinity_np(pthread_self(), sizeof(set), &set) == 0;
}

bool BindCurrentThreadMemory(std::int32_t node) noexcept {
  if (node < 0) return true;
  constexpr std::size_t kBits = sizeof(unsigned long) * 8U;
  if (static_cast<std::size_t>(node) >= kBits) return false;
  const unsigned long mask = 1UL << static_cast<unsigned>(node);
  return ::syscall(SYS_set_mempolicy, MPOL_BIND, &mask, kBits) == 0;
}

void PrefaultOwnerStack() noexcept {
  std::array<std::byte, 64U << 10U> pages{};
  volatile std::byte* memory = pages.data();
  for (std::size_t offset = 0; offset < pages.size(); offset += 4096U)
    memory[offset] = std::byte{0};
}

template <std::size_t Capacity>
void CopyText(std::string_view text,
              std::array<char, Capacity>& output) {
  if (text.empty() || text.size() >= Capacity)
    throw std::invalid_argument("invalid instrument text");
  std::memcpy(output.data(), text.data(), text.size());
}

oms::api::InstrumentInit CatalogInstrument(
    const utils::md::InstrumentCatalog& source) {
  oms::api::InstrumentInit result;
  auto& instrument = result.instrument;
  instrument.instrument_id = source.instrument_id;
  instrument.venue = source.venue;
  instrument.product_type = source.product_type;
  instrument.price_scale = source.price_scale;
  instrument.quantity_scale = source.quantity_scale;
  instrument.contract_multiplier_scale = source.contract_multiplier_scale;
  instrument.flags = source.flags;
  instrument.tick_size = source.tick_size;
  instrument.lot_size = source.lot_size;
  instrument.contract_multiplier = source.contract_multiplier;
  const auto copy = []<std::size_t To, std::size_t From>(
                        const std::array<char, From>& input,
                        std::array<char, To>& output) {
    const auto end = std::find(input.begin(), input.end(), '\0');
    const std::size_t size =
        std::min<std::size_t>(static_cast<std::size_t>(end - input.begin()),
                              output.size() - 1);
    std::copy_n(input.begin(), size, output.begin());
  };
  copy(source.base_asset, instrument.base_asset);
  copy(source.quote_asset, instrument.quote_asset);
  copy(source.settle_asset, instrument.settle_asset);
  copy(source.canonical_symbol, instrument.canonical_symbol);
  copy(source.venue_symbol, instrument.venue_symbol);
  const std::string key =
      std::to_string(static_cast<unsigned>(source.venue)) + ":" +
      std::to_string(static_cast<unsigned>(source.product_type)) + ":" +
      std::string(instrument.canonical_symbol.data());
  CopyText(key, instrument.instrument_key);
  result.minimum_order_size = source.lot_size > 0 ? source.lot_size : 1;
  if (source.venue == utils::md::Venue::Polymarket) {
    const auto text = [](const auto& value) {
      const auto end = std::find(value.begin(), value.end(), '\0');
      return std::string_view(
          value.data(), static_cast<std::size_t>(end - value.begin()));
    };
    if (!ParseUint256(text(source.condition_id),
                      result.polymarket_condition_id) ||
        !ParseUint256(text(source.venue_symbol),
                      result.polymarket_token_id))
      throw std::invalid_argument("invalid Polymarket catalog routing");
    const auto outcome = text(source.outcome);
    result.polymarket_outcome =
        outcome == "UP" || outcome == "Up" || outcome == "YES" ||
                outcome == "Yes"
            ? oms::api::PolymarketOutcome::Yes
            : oms::api::PolymarketOutcome::No;
    result.polymarket_negative_risk = source.negative_risk != 0;
    result.polymarket_signature_type = source.signature_type;
  }
  return result;
}

}  // namespace

class RuntimeCore final : public detail::Runtime {
 public:
  RuntimeCore(StrategyFrameConfig config,
              const detail::CallbackTable& callbacks, void* strategy)
      : config_(std::move(config)),
        callbacks_(callbacks),
        strategy_(strategy),
        orders_(config_.capacities.order_table),
        positions_(config_.capacities.position_table,
                   config_.capacities.fill_dedup),
        accounts_(orders_, positions_),
        timers_(config_.capacities.timer_table),
        mds_(config_.mds, MakeMdsSink()),
        timer_fd_(::timerfd_create(CLOCK_MONOTONIC,
                                   TFD_NONBLOCK | TFD_CLOEXEC)) {
    instruments_.reserve(config_.capacities.market_data_queue);
    startup_updates_.reserve(config_.capacities.update_queue);
  }

  ~RuntimeCore() override {
    mds_.stop();
    if (execution_) (void)execution_->shutdown();
    if (timer_fd_ >= 0) ::close(timer_fd_);
  }

  Error run() noexcept override {
    if (owner_ != std::thread::id{}) return Error::InvalidState;
    owner_ = std::this_thread::get_id();
    if (timer_fd_ < 0) return Error::Internal;
    if (!BindCurrentThread(config_.strategy_cpu) &&
        config_.strictness == ConfigStrictness::Strict)
      return Error::InvalidConfig;
    if (!BindCurrentThreadMemory(config_.numa_node) &&
        config_.strictness == ConfigStrictness::Strict)
      return Error::InvalidConfig;
    if (config_.memory.lock_pages &&
        ::mlockall(MCL_CURRENT | MCL_FUTURE) != 0 &&
        config_.memory.strict)
      return Error::InvalidConfig;
    if (config_.memory.prefault) PrefaultOwnerStack();
    const Error mds_error = mds_.start();
    if (mds_error != Error::Ok) return mds_error;

    if (config_.mds.required_instruments.empty()) {
      // Late SharedRing readers cause producers to republish metadata. Drain
      // the initial burst before freezing the OMS instrument registry.
      for (std::size_t rounds = 0; rounds < 64; ++rounds) {
        std::size_t count{};
        const Error error = mds_.poll(config_.event_budget, count);
        if (error != Error::Ok) return error;
        if (count == 0) break;
      }
    } else {
      const std::uint64_t started = MonotonicNowNs();
      const std::uint64_t deadline =
          started > std::numeric_limits<std::uint64_t>::max() -
                        config_.startup_timeout_ns
              ? std::numeric_limits<std::uint64_t>::max()
              : started + config_.startup_timeout_ns;
      while (!required_catalogs_ready()) {
        std::size_t count{};
        const Error error = mds_.poll(config_.event_budget, count);
        if (error != Error::Ok) return error;
        if (required_catalogs_ready()) break;
        if (MonotonicNowNs() >= deadline) return Error::NotReady;
        if (count == 0) {
          if (config_.idle_policy == IdlePolicy::BusySpin)
            continue;
          if (config_.idle_policy == IdlePolicy::Adaptive)
            std::this_thread::yield();
          else
            std::this_thread::sleep_for(std::chrono::milliseconds(1));
        }
      }
    }
    if (!instruments_.empty()) {
      const Error error = create_execution();
      if (error != Error::Ok) return error;
    }

    StrategyContext context(this, &ContextOps());
    initialized_ = true;
    if (!callbacks_.init || !callbacks_.init(strategy_, context)) {
      ++metrics_.callback_failures;
      return Error::CallbackFailed;
    }
    if (callbacks_.catalog) {
      for (auto& entry : catalogs_) {
        if (entry.notified_generation == entry.generation) continue;
        const auto update = CatalogInfo(entry.catalog, entry.generation);
        if (!callbacks_.catalog(strategy_, update)) {
          fail_callback();
          return Error::CallbackFailed;
        }
        entry.notified_generation = entry.generation;
      }
    }
    for (const auto& update : startup_updates_) apply_oms_update(update);
    startup_updates_.clear();
    if (callback_failed_) return Error::CallbackFailed;

    Error result = Error::Ok;
    while (!stop_.load(std::memory_order_acquire)) {
      bool progress = false;
      if (execution_) {
        if (config_.threading == ThreadingMode::SingleThread) {
          const auto error = execution_->service_io(0);
          if (error != oms::api::Error::Ok) {
            result = FromOms(error);
            break;
          }
        }
        const std::size_t drained = execution_->drain_updates(
            kLane, &RuntimeCore::OnOmsUpdate, this, config_.event_budget);
        progress = progress || drained != 0;
      }

      const std::size_t timer_count = dispatch_timers();
      progress = progress || timer_count != 0;

      std::size_t market_count{};
      result = mds_.poll(config_.event_budget, market_count);
      if (result != Error::Ok) {
        if (callback_failed_) result = Error::CallbackFailed;
        break;
      }
      metrics_.market_updates += market_count;
      progress = progress || market_count != 0;
      if (!execution_ && !instruments_.empty()) {
        result = create_execution();
        if (result != Error::Ok) break;
      }
      if (callback_failed_) {
        result = Error::CallbackFailed;
        break;
      }
      if (!progress) idle();
    }

    mds_.stop();
    if (execution_) {
      drain_shutdown_updates();
      const Error shutdown = FromOms(execution_->shutdown());
      drain_shutdown_updates();
      if (result == Error::Ok && shutdown != Error::Ok) result = shutdown;
    }
    return result;
  }

  void request_stop() noexcept override {
    stop_.store(true, std::memory_order_release);
  }

  RuntimeMetrics metrics() const noexcept override {
    RuntimeMetrics result = metrics_;
    if (execution_) {
      const auto oms_metrics = execution_->metrics();
      if (oms_metrics.lane_count != 0) {
        result.command_queue_high_water =
            oms_metrics.lanes[0].command_queue.high_water;
        result.update_queue_high_water =
            oms_metrics.lanes[0].update_queue.high_water;
      }
      result.unmatched_venue_events =
          oms_metrics.unmatched_venue_events;
      result.unmatched_fills = oms_metrics.unmatched_fills;
    }
    return result;
  }

 private:
  [[nodiscard]] bool required_catalogs_ready() const noexcept {
    return std::all_of(
        config_.mds.required_instruments.begin(),
        config_.mds.required_instruments.end(),
        [this](const auto& selector) {
          return static_cast<bool>(find_instrument(selector));
        });
  }

  struct TimerSlot {
    std::uint64_t deadline{};
    std::uint64_t interval{};
    std::uint32_t generation{1};
    bool active{};
  };

  struct CatalogEntry {
    utils::md::InstrumentCatalog catalog{};
    std::uint32_t generation{};
    std::uint32_t notified_generation{};
  };

  struct PendingQuery {
    QueryToken token{};
    AccountId account_id{};
    QueryComplete::Kind kind{QueryComplete::Kind::OpenOrders};
    std::vector<QueriedOrderView> orders;
    std::vector<QueriedPositionView> positions;
  };

  MarketDataSource::Sink MakeMdsSink() noexcept {
    return {this,
            &RuntimeCore::OnInstrument,
            &RuntimeCore::OnCatalog,
            &RuntimeCore::OnBbo,
            &RuntimeCore::OnBook,
            &RuntimeCore::OnAggBbo,
            &RuntimeCore::OnAggBook,
            &RuntimeCore::OnGap};
  }

  Error create_execution() noexcept {
    if (execution_ || instruments_.empty()) return Error::InvalidState;
    oms::api::RuntimeConfig runtime{};
    runtime.mode = config_.threading == ThreadingMode::SingleThread
                       ? oms::api::ExecutionMode::Inline
                       : oms::api::ExecutionMode::DedicatedIo;
    runtime.lane_count = 1;
    runtime.io_cpu_id = config_.io_cpu;
    runtime.deadline_capacity = config_.capacities.timer_table;
    runtime.order_capacity = config_.capacities.order_table;
    runtime.fill_dedup_capacity = config_.capacities.fill_dedup;
    runtime.pending_event_capacity = config_.capacities.update_queue;
    runtime.lanes[0] = {kLane, config_.capacities.command_queue,
                        config_.capacities.update_queue,
                        config_.strategy_cpu};
    runtime = oms::api::NormalizeRuntimeConfig(runtime);

    const bool has_live_venue =
        std::any_of(config_.venues.begin(), config_.venues.end(),
                    [](const auto& venue) { return venue.enabled; });
    if (!has_live_venue) {
      auto created =
          oms::api::ExecutionChannel::Create(runtime, instruments_);
      if (!created) return FromOms(created.error);
      execution_ = std::move(created.value);
    } else {
      oms::api::ExecutionChannelConfig live{};
      live.runtime = runtime;
      live.event_budget = config_.event_budget;
      live.socket.tcp_nodelay = config_.socket.tcp_nodelay;
      live.socket.receive_buffer_bytes =
          config_.socket.receive_buffer_bytes;
      live.socket.send_buffer_bytes = config_.socket.send_buffer_bytes;
      live.socket.busy_poll_us = config_.socket.busy_poll_us;
      for (const auto& venue : config_.venues) {
        if (!venue.enabled) continue;
        const auto endpoint = [](const EndpointConfig& input) {
          return oms::api::EndpointOverride{input.host, input.service};
        };
        if (venue.kind == VenueExecutionConfig::Kind::BinanceSpot ||
            venue.kind == VenueExecutionConfig::Kind::BinanceUsdm) {
          auto& output =
              venue.kind == VenueExecutionConfig::Kind::BinanceSpot
                  ? live.binance_spot
                  : live.binance_usdm;
          output.account_id = venue.account_id;
          output.enabled = true;
          output.endpoints.rest = endpoint(venue.rest);
          output.endpoints.websocket = endpoint(venue.user_websocket);
          output.trading_websocket = endpoint(venue.trading_websocket);
          const char* key = Environment(venue.api_key_env);
          const char* secret = Environment(venue.secret_env);
          if (!key || !secret) return Error::InvalidConfig;
          output.credentials = {key, secret};
        } else {
          auto& output = live.polymarket;
          output.account_id = venue.account_id;
          output.enabled = true;
          output.endpoints.rest = endpoint(venue.rest);
          output.endpoints.websocket = endpoint(venue.user_websocket);
          const char* signer = Environment(venue.signer_address_env);
          const char* funder = Environment(venue.funder_address_env);
          const char* private_key = Environment(venue.private_key_env);
          const char* key = Environment(venue.api_key_env);
          const char* secret = Environment(venue.secret_env);
          const char* passphrase = Environment(venue.passphrase_env);
          if (!signer || !funder || !private_key || !key || !secret ||
              !passphrase)
            return Error::InvalidConfig;
          output.credentials = {signer, funder, private_key,
                                key,    secret, passphrase};
        }
      }
      auto created =
          oms::api::ExecutionChannel::Create(live, instruments_);
      if (!created) return FromOms(created.error);
      execution_ = std::move(created.value);
    }
    const auto initialized =
        execution_->initialize_lane(kLane, config_.session_epoch);
    if (!initialized) return FromOms(initialized.error);
    return prewarm_execution();
  }

  static void CaptureStartupUpdate(
      void* context, const oms::api::RuntimeUpdate& update) noexcept {
    auto& self = *static_cast<RuntimeCore*>(context);
    if (self.startup_updates_.size() <
        self.startup_updates_.capacity()) {
      self.startup_updates_.push_back(update);
    } else {
      ++self.metrics_.out_of_order_updates;
    }
  }

  Error prewarm_execution() noexcept {
    if (config_.venues.empty()) return Error::Ok;
    const std::uint64_t started = MonotonicNowNs();
    const std::uint64_t deadline =
        started > std::numeric_limits<std::uint64_t>::max() -
                      config_.startup_timeout_ns
            ? std::numeric_limits<std::uint64_t>::max()
            : started + config_.startup_timeout_ns;
    for (;;) {
      if (config_.threading == ThreadingMode::SingleThread) {
        const auto serviced = execution_->service_io(0);
        if (serviced != oms::api::Error::Ok &&
            serviced != oms::api::Error::NotReady)
          return FromOms(serviced);
      }
      (void)execution_->drain_updates(
          kLane, &RuntimeCore::CaptureStartupUpdate, this,
          config_.event_budget);
      bool ready = true;
      for (const auto& venue : config_.venues) {
        if (!venue.enabled) continue;
        oms::exchange::AdapterKind kind{};
        switch (venue.kind) {
          case VenueExecutionConfig::Kind::BinanceSpot:
            kind = oms::exchange::AdapterKind::BinanceSpot;
            break;
          case VenueExecutionConfig::Kind::BinanceUsdm:
            kind = oms::exchange::AdapterKind::BinanceUsdm;
            break;
          case VenueExecutionConfig::Kind::Polymarket:
            kind = oms::exchange::AdapterKind::Polymarket;
            break;
        }
        const auto status = execution_->venue_status(kind);
        if (!status) return FromOms(status.error);
        if (status.value.status == oms::exchange::AdapterStatus::Failed)
          return Error::NotReady;
        ready = ready &&
                status.value.status == oms::exchange::AdapterStatus::Ready;
      }
      if (ready) return Error::Ok;
      if (MonotonicNowNs() >= deadline) return Error::NotReady;
      if (config_.idle_policy == IdlePolicy::BusySpin)
        continue;
      if (config_.idle_policy == IdlePolicy::Adaptive)
        std::this_thread::yield();
      else
        std::this_thread::sleep_for(std::chrono::milliseconds(1));
    }
  }

  static bool OnInstrument(void* context,
                           const utils::md::Instrument& value) noexcept {
    auto& self = *static_cast<RuntimeCore*>(context);
    const auto found = std::find_if(
        self.instruments_.begin(), self.instruments_.end(),
        [&](const auto& current) {
          return current.instrument.instrument_id == value.instrument_id;
        });
    // InstrumentUpdate is common market metadata, not an execution-routing
    // source. A catalog must establish the identity before this update can
    // enrich it.
    if (found == self.instruments_.end()) return true;
    const auto key = found->instrument.instrument_key;
    const auto canonical = found->instrument.canonical_symbol;
    const auto venue_symbol = found->instrument.venue_symbol;
    found->instrument = value;
    found->instrument.instrument_key = key;
    if (found->instrument.canonical_symbol[0] == '\0')
      found->instrument.canonical_symbol = canonical;
    if (found->instrument.venue_symbol[0] == '\0')
      found->instrument.venue_symbol = venue_symbol;
    return true;
  }

  static InstrumentCatalogInfo CatalogInfo(
      const utils::md::InstrumentCatalog& value,
      std::uint32_t generation) noexcept {
    const auto copy = []<typename Output, std::size_t Size>(
                          const std::array<char, Size>& source) {
      Output output{};
      std::size_t length = 0;
      while (length < source.size() && source[length] != '\0') ++length;
      const auto count =
          std::min<std::size_t>(length, sizeof(output.value));
      if (count != 0) std::memcpy(output.value, source.data(), count);
      output.length = static_cast<std::uint16_t>(count);
      return output;
    };
    InstrumentCatalogInfo result;
    result.instrument_id = value.instrument_id;
    result.venue = static_cast<Venue>(value.venue);
    result.product = static_cast<ProductType>(value.product_type);
    result.canonical_symbol =
        copy.template operator()<InstrumentSymbol>(value.canonical_symbol);
    result.market_slug =
        copy.template operator()<MarketSlug>(value.market_slug);
    result.venue_symbol =
        copy.template operator()<VenueInstrumentSymbol>(value.venue_symbol);
    result.condition_id =
        copy.template operator()<ConditionId>(value.condition_id);
    result.outcome = copy.template operator()<OutcomeName>(value.outcome);
    result.expiry_time_ns = value.expiry_unix_ns;
    result.generation = generation;
    result.price_scale = value.price_scale;
    result.quantity_scale = value.quantity_scale;
    result.signature_type = value.signature_type;
    result.negative_risk = value.negative_risk;
    result.tick_size = value.tick_size;
    result.lot_size = value.lot_size;
    result.contract_multiplier = value.contract_multiplier;
    return result;
  }

  static bool OnCatalog(void* context,
                        const utils::md::InstrumentCatalog& value,
                        std::uint32_t generation) noexcept {
    auto& self = *static_cast<RuntimeCore*>(context);
    for (const auto& configured : self.config_.instruments) {
      if (configured.instrument_id != value.instrument_id) continue;
      const auto text = [](const auto& field) {
        const auto end = std::find(field.begin(), field.end(), '\0');
        return std::string_view(
            field.data(), static_cast<std::size_t>(end - field.begin()));
      };
      if (configured.venue != static_cast<Venue>(value.venue) ||
          configured.product !=
              static_cast<ProductType>(value.product_type) ||
          configured.symbol != text(value.canonical_symbol) ||
          configured.price_scale != value.price_scale ||
          configured.quantity_scale != value.quantity_scale ||
          configured.tick_size != value.tick_size ||
          configured.lot_size != value.lot_size) {
        return false;
      }
    }
    auto found = std::find_if(
        self.catalogs_.begin(), self.catalogs_.end(),
        [&](const auto& entry) {
          return entry.catalog.instrument_id == value.instrument_id;
        });
    bool changed = false;
    if (found == self.catalogs_.end()) {
      try {
        self.catalogs_.push_back({value, generation, 0});
        found = std::prev(self.catalogs_.end());
        changed = true;
      } catch (...) {
        return false;
      }
    } else {
      if (generation < found->generation) return true;
      if (generation == found->generation) {
        return std::memcmp(&found->catalog, &value, sizeof(value)) == 0;
      }
      found->catalog = value;
      found->generation = generation;
      changed = true;
    }
    try {
      const auto prepared = CatalogInstrument(value);
      auto instrument = std::find_if(
          self.instruments_.begin(), self.instruments_.end(),
          [&](const auto& current) {
            return current.instrument.instrument_id ==
                   value.instrument_id;
          });
      if (instrument == self.instruments_.end()) {
        self.instruments_.push_back(prepared);
      } else {
        *instrument = prepared;
      }
    } catch (...) {
      return false;
    }
    if (changed && self.initialized_ && self.callbacks_.catalog) {
      const auto update = CatalogInfo(value, generation);
      if (!self.callbacks_.catalog(self.strategy_, update)) return false;
      found->notified_generation = generation;
    }
    const std::uint64_t now = WallNowNs();
    std::vector<InstrumentId> retired;
    try {
      for (const auto& entry : self.catalogs_) {
        const auto& catalog = entry.catalog;
        if (catalog.instrument_id == value.instrument_id ||
            catalog.venue != value.venue ||
            catalog.product_type != value.product_type ||
            catalog.canonical_symbol != value.canonical_symbol ||
            catalog.expiry_unix_ns == 0 ||
            catalog.expiry_unix_ns > now) {
          continue;
        }
        retired.push_back(catalog.instrument_id);
      }
    } catch (...) {
      return false;
    }
    for (const InstrumentId instrument_id : retired) {
      self.positions_.retire_instrument(instrument_id);
      self.mds_.retire_instrument(instrument_id);
      self.catalogs_.erase(
          std::remove_if(
              self.catalogs_.begin(), self.catalogs_.end(),
              [instrument_id](const auto& entry) {
                return entry.catalog.instrument_id == instrument_id;
              }),
          self.catalogs_.end());
      self.instruments_.erase(
          std::remove_if(
              self.instruments_.begin(), self.instruments_.end(),
              [instrument_id](const auto& instrument) {
                return instrument.instrument.instrument_id == instrument_id;
              }),
          self.instruments_.end());
    }
    return true;
  }

  template <typename Update, typename Callback>
  static bool DispatchMarket(void* context, const Update& update,
                             Callback callback) noexcept {
    auto& self = *static_cast<RuntimeCore*>(context);
    if (!self.initialized_) return true;
    if (!callback) return true;
    bool sample = false;
    if (++self.metric_sequence_ >= self.config_.metric_sample_rate) {
      self.metric_sequence_ = 0;
      sample = true;
    }
    const std::uint64_t started = sample ? MonotonicNowNs() : 0;
    if (sample && update.header.receive_tsc != 0) {
      const std::uint64_t current_tsc = ReadTsc();
      if (current_tsc >= update.header.receive_tsc) {
        const std::uint64_t cycles =
            current_tsc - update.header.receive_tsc;
        ++self.metrics_.tick_to_callback_samples;
        self.metrics_.tick_to_callback_total_cycles += cycles;
        self.metrics_.tick_to_callback_max_cycles =
            std::max(self.metrics_.tick_to_callback_max_cycles, cycles);
      }
    }
    const bool succeeded = callback(self.strategy_, update);
    if (sample) {
      const std::uint64_t elapsed = MonotonicNowNs() - started;
      ++self.metrics_.callback_samples;
      self.metrics_.callback_total_ns += elapsed;
      self.metrics_.callback_max_ns =
          std::max(self.metrics_.callback_max_ns, elapsed);
    }
    if (succeeded) return true;
    ++self.metrics_.callback_failures;
    self.callback_failed_ = true;
    return false;
  }

  static bool OnBbo(void* context, const BboUpdate& update) noexcept {
    return DispatchMarket(context, update,
                          static_cast<RuntimeCore*>(context)->callbacks_.bbo);
  }
  static bool OnBook(void* context,
                     const OrderBookUpdate& update) noexcept {
    return DispatchMarket(context, update,
                          static_cast<RuntimeCore*>(context)->callbacks_.book);
  }
  static bool OnAggBbo(void* context,
                       const AggBboUpdate& update) noexcept {
    return DispatchMarket(
        context, update,
        static_cast<RuntimeCore*>(context)->callbacks_.agg_bbo);
  }
  static bool OnAggBook(void* context,
                        const AggOrderBookUpdate& update) noexcept {
    return DispatchMarket(
        context, update,
        static_cast<RuntimeCore*>(context)->callbacks_.agg_book);
  }
  static void OnGap(void* context, std::size_t) noexcept {
    ++static_cast<RuntimeCore*>(context)->metrics_.out_of_order_updates;
  }

  static void OnOmsUpdate(void* context,
                          const oms::api::RuntimeUpdate& update) noexcept {
    static_cast<RuntimeCore*>(context)->apply_oms_update(update);
  }

  void apply_oms_update(const oms::api::RuntimeUpdate& value) noexcept {
    if (callback_failed_) return;
    if (value.kind == oms::api::RuntimeUpdateKind::Control) {
      OmsStatusUpdate update;
      update.kind =
          value.control.kind == oms::api::RuntimeControlKind::VenueStatus
              ? OmsStatusUpdate::Kind::VenueStatus
              : OmsStatusUpdate::Kind::ReconcileComplete;
      update.adapter_kind = value.control.adapter_kind;
      update.adapter_status = value.control.adapter_status;
      update.error = static_cast<std::int32_t>(value.control.error);
      update.generation = value.control.generation;
      update.event_time_ns = value.control.event_time_ns;
      if (update.kind == OmsStatusUpdate::Kind::VenueStatus &&
          update.adapter_kind < latest_status_.size()) {
        latest_status_[update.adapter_kind] = update;
        has_status_[update.adapter_kind] = true;
      }
      if (update.kind == OmsStatusUpdate::Kind::ReconcileComplete)
        ++metrics_.reconcile_count;
      if (callbacks_.status && !callbacks_.status(strategy_, update))
        fail_callback();
      return;
    }
    if (value.kind == oms::api::RuntimeUpdateKind::OpenOrderSnapshot ||
        value.kind == oms::api::RuntimeUpdateKind::PositionSnapshot ||
        value.kind == oms::api::RuntimeUpdateKind::QueryComplete) {
      const QueryToken token =
          FromOms(value.kind == oms::api::RuntimeUpdateKind::OpenOrderSnapshot
                      ? value.open_order.query_token
                      : (value.kind ==
                                 oms::api::RuntimeUpdateKind::PositionSnapshot
                             ? value.position.query_token
                             : value.query_complete.query_token));
      const auto pending = std::find_if(
          pending_queries_.begin(), pending_queries_.end(),
          [&](const PendingQuery& query) { return query.token == token; });
      if (pending == pending_queries_.end()) return;
      if (value.kind == oms::api::RuntimeUpdateKind::OpenOrderSnapshot) {
        QueriedOrderView item{};
        item.account_id = value.open_order.account_id;
        item.instrument_id = value.open_order.instrument_id;
        item.venue_order_id =
            CopyId<VenueOrderId>(value.open_order.venue_order_id);
        item.status = FromOms(value.open_order.status);
        item.side = static_cast<Side>(value.open_order.side);
        item.quantity = FromOms(value.open_order.quantity);
        item.price = FromOms(value.open_order.price);
        item.matched_quantity =
            FromOms(value.open_order.matched_quantity);
        item.remaining_quantity =
            FromOms(value.open_order.remaining_quantity);
        pending->orders.push_back(item);
        return;
      }
      if (value.kind == oms::api::RuntimeUpdateKind::PositionSnapshot) {
        pending->positions.push_back(
            {value.position.account_id, value.position.instrument_id,
             FromOms(value.position.quantity)});
        return;
      }
      if (pending->kind == QueryComplete::Kind::OpenOrders) {
        OpenOrdersSnapshot snapshot{pending->token, pending->account_id,
                                    pending->orders};
        if (callbacks_.open_orders_snapshot &&
            !callbacks_.open_orders_snapshot(strategy_, snapshot))
          fail_callback();
      } else {
        PositionsSnapshot snapshot{pending->token, pending->account_id,
                                   pending->positions};
        if (callbacks_.positions_snapshot &&
            !callbacks_.positions_snapshot(strategy_, snapshot))
          fail_callback();
      }
      QueryComplete complete{
          pending->token, pending->account_id, pending->kind, 0,
          static_cast<std::int32_t>(FromOms(value.query_complete.error))};
      if (callbacks_.query_complete &&
          !callbacks_.query_complete(strategy_, complete))
        fail_callback();
      pending_queries_.erase(pending);
      return;
    }

    ExecutionUpdate update;
    update.event_time_ns = value.published_time_ns;
    if (value.kind == oms::api::RuntimeUpdateKind::Order) {
      update.kind = ExecutionUpdate::Kind::Order;
      update.status = FromOms(value.order.status);
      update.update_type = static_cast<std::uint8_t>(value.order.type);
      update.token = FromOms(value.order.token);
      update.client_order_id =
          CopyId<ClientOrderId>(value.order.client_order_id);
      update.venue_order_id =
          CopyId<VenueOrderId>(value.order.venue_order_id);
    } else if (value.kind == oms::api::RuntimeUpdateKind::Fill) {
      update.kind = ExecutionUpdate::Kind::Fill;
      update.status = value.fill.remaining_quantity.value == 0
                          ? OrderStatus::Filled
                          : OrderStatus::PartiallyFilled;
      update.token = FromOms(value.fill.token);
      update.client_order_id =
          CopyId<ClientOrderId>(value.fill.client_order_id);
      update.venue_order_id =
          CopyId<VenueOrderId>(value.fill.venue_order_id);
      update.trade_id = CopyId<TradeId>(value.fill.trade_id);
      update.fill_quantity = FromOms(value.fill.quantity);
      update.fill_price = FromOms(value.fill.price);
      update.cumulative_quantity =
          FromOms(value.fill.cumulative_quantity);
      update.remaining_quantity = FromOms(value.fill.remaining_quantity);
    } else {
      update.kind = ExecutionUpdate::Kind::CommandResult;
      update.update_type =
          static_cast<std::uint8_t>(value.command_result.kind);
      update.error =
          static_cast<std::int32_t>(value.command_result.error);
      if (value.command_result.kind ==
          oms::api::RuntimeCommandResultKind::Cancel) {
        update.token =
            FromOms(value.command_result.correlation.target_token);
      } else if (value.command_result.kind ==
                 oms::api::RuntimeCommandResultKind::RebindInstrument) {
        update.token =
            FromOms(value.command_result.rebind.request_token);
        update.instrument_id =
            value.command_result.rebind.instrument_id;
      } else {
        update.token =
            FromOms(value.command_result.correlation.request_token);
      }
    }

    const auto existing = orders_.find(update.token);
    if (existing) {
      update.account_id = existing.value.account_id;
      update.instrument_id = existing.value.instrument_id;
      if (update.kind == ExecutionUpdate::Kind::Fill) {
        const Error position_error = positions_.apply_fill(
            existing.value.account_id, existing.value.instrument_id,
            existing.value.side, PositionSide::Net, update.fill_quantity,
            update.trade_id, value.published_time_ns);
        if (position_error == Error::InvalidState)
          ++metrics_.duplicate_updates;
        else if (position_error != Error::Ok)
          ++metrics_.out_of_order_updates;
      }
    }
    if (update.kind != ExecutionUpdate::Kind::CommandResult) {
      const Error applied = orders_.apply(update);
      if (applied == Error::NotFound)
        ++metrics_.duplicate_updates;
      else if (applied != Error::Ok)
        ++metrics_.out_of_order_updates;
    }
    ++metrics_.execution_updates;
    if (++execution_metric_sequence_ >= config_.metric_sample_rate) {
      execution_metric_sequence_ = 0;
      const std::uint64_t now = MonotonicNowNs();
      if (value.published_time_ns != 0 &&
          now >= value.published_time_ns) {
        const std::uint64_t elapsed = now - value.published_time_ns;
        ++metrics_.execution_dispatch_samples;
        metrics_.execution_dispatch_total_ns += elapsed;
        metrics_.execution_dispatch_max_ns =
            std::max(metrics_.execution_dispatch_max_ns, elapsed);
      }
    }
    if (callbacks_.execution && !callbacks_.execution(strategy_, update))
      fail_callback();
  }

  void fail_callback() noexcept {
    ++metrics_.callback_failures;
    callback_failed_ = true;
  }

  void drain_shutdown_updates() noexcept {
    for (std::size_t round = 0; round < 64; ++round) {
      if (config_.threading == ThreadingMode::SingleThread)
        (void)execution_->service_io(0);
      const std::size_t drained = execution_->drain_updates(
          kLane, &RuntimeCore::OnOmsUpdate, this, config_.event_budget);
      if (drained == 0) break;
    }
  }

  std::size_t dispatch_timers() noexcept {
    std::uint64_t kernel_expirations{};
    while (::read(timer_fd_, &kernel_expirations,
                  sizeof(kernel_expirations)) ==
           static_cast<ssize_t>(sizeof(kernel_expirations))) {
    }
    const std::uint64_t now = MonotonicNowNs();
    std::size_t count = 0;
    for (std::size_t i = 0; i < timers_.size(); ++i) {
      TimerSlot& timer = timers_[i];
      if (!timer.active || timer.deadline > now) continue;
      std::uint64_t expirations = 1;
      const std::uint64_t scheduled = timer.deadline;
      const TimerHandle handle{static_cast<std::uint32_t>(i),
                               timer.generation};
      if (timer.interval == 0) {
        timer.active = false;
        ++timer.generation;
        if (timer.generation == 0) ++timer.generation;
      } else {
        expirations += (now - timer.deadline) / timer.interval;
        if (expirations >
            (std::numeric_limits<std::uint64_t>::max() - timer.deadline) /
                timer.interval)
          timer.deadline = std::numeric_limits<std::uint64_t>::max();
        else
          timer.deadline += expirations * timer.interval;
      }
      TimerEvent event{handle, scheduled, now, expirations};
      ++metrics_.timer_events;
      ++count;
      if (callbacks_.timer && !callbacks_.timer(strategy_, event)) {
        fail_callback();
        break;
      }
    }
    arm_timer_fd();
    return count;
  }

  void arm_timer_fd() noexcept {
    std::uint64_t earliest = std::numeric_limits<std::uint64_t>::max();
    for (const TimerSlot& timer : timers_)
      if (timer.active) earliest = std::min(earliest, timer.deadline);
    itimerspec setting{};
    if (earliest != std::numeric_limits<std::uint64_t>::max()) {
      setting.it_value.tv_sec =
          static_cast<time_t>(earliest / 1'000'000'000ULL);
      setting.it_value.tv_nsec =
          static_cast<long>(earliest % 1'000'000'000ULL);
    }
    (void)::timerfd_settime(timer_fd_, TFD_TIMER_ABSTIME, &setting, nullptr);
  }

  void idle() const noexcept {
    if (config_.idle_policy == IdlePolicy::BusySpin) return;
    if (config_.idle_policy == IdlePolicy::Adaptive) {
      std::this_thread::yield();
      return;
    }
    std::array<pollfd, 2> descriptors{};
    nfds_t count = 0;
    descriptors[count++] = {timer_fd_, POLLIN, 0};
    if (execution_ &&
        config_.threading == ThreadingMode::MultiIoThread) {
      const int update_fd = execution_->notification_fd(kLane);
      if (update_fd >= 0) descriptors[count++] = {update_fd, POLLIN, 0};
    }
    (void)::poll(descriptors.data(), count, 1);
  }

  Result<InstrumentInfo> find_instrument(
      InstrumentId instrument_id) const noexcept {
    for (const auto& configured : instruments_) {
      const auto& instrument = configured.instrument;
      if (instrument.instrument_id != instrument_id) continue;
      InstrumentInfo result;
      result.instrument_id = instrument.instrument_id;
      result.venue = static_cast<Venue>(instrument.venue);
      result.product = static_cast<ProductType>(instrument.product_type);
      const auto& symbol = instrument.venue_symbol[0] != '\0'
                               ? instrument.venue_symbol
                               : instrument.canonical_symbol;
      std::size_t length = 0;
      while (length < symbol.size() && symbol[length] != '\0') ++length;
      const std::size_t copied =
          std::min<std::size_t>(length, sizeof(result.symbol.value));
      if (copied != 0)
        std::memcpy(result.symbol.value, symbol.data(), copied);
      result.symbol.length = static_cast<std::uint16_t>(copied);
      result.price_scale = instrument.price_scale;
      result.quantity_scale = instrument.quantity_scale;
      result.tick_size = instrument.tick_size;
      result.lot_size = instrument.lot_size;
      return {result, Error::Ok};
    }
    return {{}, Error::NotFound};
  }

  Result<InstrumentCatalogInfo> find_instrument_catalog(
      InstrumentId instrument_id) const noexcept {
    const auto found = std::find_if(
        catalogs_.begin(), catalogs_.end(), [&](const auto& entry) {
          return entry.catalog.instrument_id == instrument_id;
        });
    if (found == catalogs_.end()) return {{}, Error::NotFound};
    return {CatalogInfo(found->catalog, found->generation), Error::Ok};
  }

  Result<InstrumentCatalogInfo> find_instrument(
      const InstrumentSelector& selector) const noexcept {
    const CatalogEntry* best = nullptr;
    const std::uint64_t now = WallNowNs();
    for (const auto& entry : catalogs_) {
      const auto& value = entry.catalog;
      std::size_t length = 0;
      while (length < value.canonical_symbol.size() &&
             value.canonical_symbol[length] != '\0')
        ++length;
      if (static_cast<Venue>(value.venue) != selector.venue ||
          static_cast<ProductType>(value.product_type) != selector.product ||
          std::string_view(value.canonical_symbol.data(), length) !=
              selector.canonical_symbol) {
        continue;
      }
      if (value.expiry_unix_ns != 0 && value.expiry_unix_ns <= now)
        continue;
      if (best == nullptr ||
          (value.expiry_unix_ns == 0 &&
           best->catalog.expiry_unix_ns != 0) ||
          (value.expiry_unix_ns == best->catalog.expiry_unix_ns &&
           value.instrument_id < best->catalog.instrument_id) ||
          (value.expiry_unix_ns != 0 &&
           best->catalog.expiry_unix_ns != 0 &&
           value.expiry_unix_ns < best->catalog.expiry_unix_ns)) {
        best = &entry;
      }
    }
    if (best == nullptr) return {{}, Error::NotFound};
    return {CatalogInfo(best->catalog, best->generation), Error::Ok};
  }

  Result<OmsStatusUpdate> oms_status(
      std::uint8_t adapter_kind) const noexcept {
    if (adapter_kind >= latest_status_.size() ||
        !has_status_[adapter_kind])
      return {{}, Error::NotFound};
    return {latest_status_[adapter_kind], Error::Ok};
  }

  static const StrategyContext::Ops& ContextOps() noexcept {
    static const StrategyContext::Ops ops{
        [](void* state, const OrderRequest& request) noexcept {
          return static_cast<RuntimeCore*>(state)->place(request);
        },
        [](void* state, OrderToken token) noexcept {
          return static_cast<RuntimeCore*>(state)->cancel(token);
        },
        [](void* state, AccountId account) noexcept {
          return static_cast<RuntimeCore*>(state)->query(
              account, QueryComplete::Kind::OpenOrders);
        },
        [](void* state, AccountId account) noexcept {
          return static_cast<RuntimeCore*>(state)->query(
              account, QueryComplete::Kind::Positions);
        },
        [](void* state, std::uint64_t deadline,
           std::uint64_t interval) noexcept {
          return static_cast<RuntimeCore*>(state)->schedule(deadline,
                                                            interval);
        },
        [](void* state, TimerHandle handle) noexcept {
          return static_cast<RuntimeCore*>(state)->cancel_timer(handle);
        },
        [](const void* state) noexcept {
          return static_cast<const RuntimeCore*>(state)->orders_.open_orders();
        },
        [](const void* state) noexcept {
          return static_cast<const RuntimeCore*>(state)->positions_.positions();
        },
        [](const void* state, OrderToken token) noexcept {
          return static_cast<const RuntimeCore*>(state)->orders_.find(token);
        },
        [](const void* state, AccountId account, InstrumentId instrument,
           PositionSide side) noexcept {
          return static_cast<const RuntimeCore*>(state)->positions_.find(
              account, instrument, side);
        },
        [](const void* state, InstrumentId instrument) noexcept {
          return static_cast<const RuntimeCore*>(state)->find_instrument(
              instrument);
        },
        [](const void* state,
           const InstrumentSelector& selector) noexcept {
          return static_cast<const RuntimeCore*>(state)->find_instrument(
              selector);
        },
        [](const void* state, InstrumentId instrument) noexcept {
          return static_cast<const RuntimeCore*>(state)
              ->find_instrument_catalog(instrument);
        },
        [](const void* state, std::uint8_t adapter_kind) noexcept {
          return static_cast<const RuntimeCore*>(state)->oms_status(
              adapter_kind);
        },
        [](const void* state) noexcept -> const StrategyParams& {
          return static_cast<const RuntimeCore*>(state)->config_.strategy;
        },
        [](const void* state) noexcept {
          return static_cast<const RuntimeCore*>(state)->metrics();
        },
        [](const void*) noexcept { return MonotonicNowNs(); },
        [](void* state) noexcept {
          static_cast<RuntimeCore*>(state)->request_stop();
        }};
    return ops;
  }

  Result<OrderToken> place(const OrderRequest& request) noexcept {
    if (std::this_thread::get_id() != owner_)
      return {{}, Error::InvalidThread};
    if (!execution_) return {{}, Error::NotReady};
    if (!adapter_ready(request.instrument_id))
      return {{}, Error::NotReady};
    if (request.client_order_id.size() > 64)
      return {{}, Error::InvalidArgument};
    if (orders_.size() == orders_.capacity())
      return {{}, Error::CapacityExceeded};
    oms::api::NewOrderRequest value;
    value.instrument_id = request.instrument_id;
    value.side = static_cast<oms::api::Side>(request.side);
    value.type = static_cast<oms::api::OrderType>(request.type);
    value.time_in_force =
        static_cast<oms::api::TimeInForce>(request.time_in_force);
    value.flags = request.flags;
    value.quantity = ToOms(request.quantity);
    value.price = ToOms(request.price);
    value.expire_time_ns = request.expire_time_ns;
    value.client_order_id.length =
        static_cast<std::uint16_t>(request.client_order_id.size());
    if (!request.client_order_id.empty())
      std::memcpy(value.client_order_id.value.data(),
                  request.client_order_id.data(),
                  request.client_order_id.size());
    const bool sample = ++order_metric_sequence_ >=
                        config_.metric_sample_rate;
    if (sample) order_metric_sequence_ = 0;
    const std::uint64_t started = sample ? MonotonicNowNs() : 0;
    const auto catalog = find_instrument_catalog(request.instrument_id);
    if (!catalog) return {{}, catalog.error};
    oms::api::PreparedOrderRequest prepared;
    prepared.order = value;
    auto& routing = prepared.routing;
    routing.kind = catalog.value.venue == Venue::Polymarket
                       ? oms::api::ExecutionRouteKind::Polymarket
                       : oms::api::ExecutionRouteKind::Generic;
    routing.venue = static_cast<std::uint8_t>(catalog.value.venue);
    routing.product_type =
        static_cast<std::uint8_t>(catalog.value.product);
    routing.price_scale = catalog.value.price_scale;
    routing.quantity_scale = catalog.value.quantity_scale;
    routing.catalog_generation = catalog.value.generation;
    routing.signature_type = catalog.value.signature_type;
    routing.negative_risk = catalog.value.negative_risk;
    routing.tick_size = catalog.value.tick_size;
    routing.lot_size = catalog.value.lot_size;
    routing.minimum_order_size =
        catalog.value.lot_size > 0 ? catalog.value.lot_size : 1;
    routing.instrument_expiry_ns = catalog.value.expiry_time_ns;
    if (routing.kind == oms::api::ExecutionRouteKind::Polymarket) {
      const std::string_view outcome(catalog.value.outcome.value,
                                     catalog.value.outcome.length);
      routing.outcome =
          outcome == "UP" || outcome == "Up" || outcome == "YES" ||
                  outcome == "Yes"
              ? oms::api::PolymarketOutcome::Yes
              : (outcome == "DOWN" || outcome == "Down" ||
                         outcome == "NO" || outcome == "No"
                     ? oms::api::PolymarketOutcome::No
                     : oms::api::PolymarketOutcome::Unknown);
      if (!ParseUint256(
              std::string_view(catalog.value.condition_id.value,
                               catalog.value.condition_id.length),
              routing.condition_id) ||
          !ParseUint256(
              std::string_view(catalog.value.venue_symbol.value,
                               catalog.value.venue_symbol.length),
              routing.token_id)) {
        return {{}, Error::InvalidArgument};
      }
    }
    const auto submitted =
        execution_->place_prepared_order(kLane, prepared);
    if (sample) {
      const std::uint64_t elapsed = MonotonicNowNs() - started;
      ++metrics_.order_call_samples;
      metrics_.order_call_total_ns += elapsed;
      metrics_.order_call_max_ns =
          std::max(metrics_.order_call_max_ns, elapsed);
    }
    if (!submitted) return {{}, FromOms(submitted.error)};
    const OrderToken token = FromOms(submitted.value);
    const Error inserted = orders_.insert_pending(request, token);
    return inserted == Error::Ok ? Result<OrderToken>{token, Error::Ok}
                                 : Result<OrderToken>{{}, inserted};
  }

  bool adapter_ready(InstrumentId instrument_id) const noexcept {
    if (config_.venues.empty()) return true;
    const auto found = std::find_if(
        instruments_.begin(), instruments_.end(),
        [instrument_id](const auto& value) {
          return value.instrument.instrument_id == instrument_id;
        });
    if (found == instruments_.end()) return false;
    oms::exchange::AdapterKind kind{};
    if (found->instrument.venue == utils::md::Venue::Polymarket) {
      kind = oms::exchange::AdapterKind::Polymarket;
    } else if (found->instrument.venue == utils::md::Venue::Binance &&
               found->instrument.product_type ==
                   utils::md::ProductType::Spot) {
      kind = oms::exchange::AdapterKind::BinanceSpot;
    } else if (found->instrument.venue == utils::md::Venue::Binance &&
               (found->instrument.product_type ==
                    utils::md::ProductType::Perpetual ||
                found->instrument.product_type ==
                    utils::md::ProductType::Future)) {
      kind = oms::exchange::AdapterKind::BinanceUsdm;
    } else {
      return false;
    }
    const auto status = execution_->venue_status(kind);
    return status &&
           status.value.status == oms::exchange::AdapterStatus::Ready;
  }

  Result<OrderToken> cancel(OrderToken token) noexcept {
    if (std::this_thread::get_id() != owner_)
      return {{}, Error::InvalidThread};
    if (!execution_) return {{}, Error::NotReady};
    if (!orders_.find(token)) return {{}, Error::NotFound};
    const auto canceled = execution_->cancel_order(kLane, ToOms(token));
    return canceled ? Result<OrderToken>{FromOms(canceled.value), Error::Ok}
                    : Result<OrderToken>{{}, FromOms(canceled.error)};
  }

  Result<QueryToken> query(AccountId account_id,
                           QueryComplete::Kind kind) noexcept {
    if (std::this_thread::get_id() != owner_)
      return {{}, Error::InvalidThread};
    if (!execution_) return {{}, Error::NotReady};
    if (pending_queries_.size() >= config_.capacities.command_queue)
      return {{}, Error::CapacityExceeded};
    const auto submitted =
        kind == QueryComplete::Kind::OpenOrders
            ? execution_->query_open_orders(kLane, account_id)
            : execution_->query_positions(kLane, account_id);
    if (!submitted) return {{}, FromOms(submitted.error)};
    PendingQuery pending{};
    pending.token = FromOms(submitted.value);
    pending.account_id = account_id;
    pending.kind = kind;
    pending.orders.reserve(64);
    pending.positions.reserve(64);
    pending_queries_.push_back(std::move(pending));
    return {FromOms(submitted.value), Error::Ok};
  }

  Result<TimerHandle> schedule(std::uint64_t deadline,
                               std::uint64_t interval) noexcept {
    if (std::this_thread::get_id() != owner_)
      return {{}, Error::InvalidThread};
    if (deadline == 0) return {{}, Error::InvalidArgument};
    for (std::size_t i = 0; i < timers_.size(); ++i) {
      auto& timer = timers_[i];
      if (timer.active) continue;
      timer.deadline = deadline;
      timer.interval = interval;
      timer.active = true;
      arm_timer_fd();
      return {{static_cast<std::uint32_t>(i), timer.generation},
              Error::Ok};
    }
    return {{}, Error::CapacityExceeded};
  }

  Error cancel_timer(TimerHandle handle) noexcept {
    if (std::this_thread::get_id() != owner_) return Error::InvalidThread;
    if (handle.slot >= timers_.size()) return Error::NotFound;
    auto& timer = timers_[handle.slot];
    if (!timer.active || timer.generation != handle.generation)
      return Error::NotFound;
    timer.active = false;
    ++timer.generation;
    if (timer.generation == 0) ++timer.generation;
    arm_timer_fd();
    return Error::Ok;
  }

  StrategyFrameConfig config_;
  detail::CallbackTable callbacks_;
  void* strategy_{};
  OrderManager orders_;
  PositionManager positions_;
  AccountManager accounts_;
  std::vector<TimerSlot> timers_;
  MarketDataSource mds_;
  std::vector<oms::api::InstrumentInit> instruments_;
  std::vector<CatalogEntry> catalogs_;
  std::vector<oms::api::RuntimeUpdate> startup_updates_;
  std::vector<PendingQuery> pending_queries_;
  std::unique_ptr<oms::api::ExecutionChannel> execution_;
  int timer_fd_{-1};
  std::atomic<bool> stop_{false};
  std::thread::id owner_{};
  RuntimeMetrics metrics_{};
  std::array<OmsStatusUpdate, 8> latest_status_{};
  std::array<bool, 8> has_status_{};
  bool initialized_{};
  bool callback_failed_{};
  std::uint32_t metric_sequence_{};
  std::uint32_t execution_metric_sequence_{};
  std::uint32_t order_metric_sequence_{};
};

namespace detail {

Result<std::unique_ptr<Runtime>> make_runtime(
    StrategyFrameConfig config, const CallbackTable& callbacks,
    void* strategy) noexcept {
  if (!strategy || !callbacks.init || config.event_budget == 0 ||
      config.capacities.timer_table == 0) {
    return {{}, Error::InvalidConfig};
  }
  try {
    return {std::make_unique<RuntimeCore>(std::move(config), callbacks,
                                          strategy),
            Error::Ok};
  } catch (...) {
    return {{}, Error::Internal};
  }
}

}  // namespace detail
}  // namespace strategyframe

#include <array>
#include <atomic>
#include <cstdint>
#include <cstdlib>
#include <cstring>
#include <fstream>
#include <limits>
#include <new>
#include <stdexcept>
#include <string>
#include <string_view>
#include <vector>

#include "oms/execution_directory.h"
#include "oms/state_engine.h"
#include "utils/md/symbol.h"

namespace {
std::atomic<bool> track_allocations{false};
std::atomic<std::size_t> tracked_allocations{0};
}

void* operator new(std::size_t size) {
  if (track_allocations.load(std::memory_order_relaxed))
    tracked_allocations.fetch_add(1, std::memory_order_relaxed);
  if (void* memory = std::malloc(size == 0 ? 1 : size); memory != nullptr)
    return memory;
  throw std::bad_alloc();
}

void* operator new[](std::size_t size) { return ::operator new(size); }
void operator delete(void* memory) noexcept { std::free(memory); }
void operator delete[](void* memory) noexcept { std::free(memory); }
void operator delete(void* memory, std::size_t) noexcept { std::free(memory); }
void operator delete[](void* memory, std::size_t) noexcept {
  std::free(memory);
}

namespace {

#define REQUIRE(condition)                                                     \
  do {                                                                         \
    if (!(condition))                                                          \
      throw std::runtime_error(std::string("require failed: ") + #condition +  \
                               " at line " + std::to_string(__LINE__));        \
  } while (false)

template <typename Id>
Id MakeId(std::string_view text) {
  REQUIRE(!text.empty());
  REQUIRE(text.size() <= Id{}.value.size());
  Id id{};
  std::memcpy(id.value.data(), text.data(), text.size());
  id.length = static_cast<std::uint16_t>(text.size());
  return id;
}

oms::api::RequestToken Token(std::uint64_t sequence) {
  return {7, 11, sequence};
}

utils::md::Instrument Instrument(std::uint32_t id,
                                 std::string_view key = "6:4:YES") {
  utils::md::Instrument instrument{};
  instrument.instrument_id = id;
  instrument.venue = utils::md::Venue::Polymarket;
  instrument.product_type = utils::md::ProductType::BinaryOption;
  instrument.price_scale = 4;
  instrument.quantity_scale = 3;
  instrument.tick_size = 1;
  instrument.lot_size = 1;
  std::memcpy(instrument.instrument_key.data(), key.data(), key.size());
  return instrument;
}

oms::api::NewOrderRequest Request(std::uint64_t sequence,
                                  std::int64_t quantity = 10) {
  oms::api::NewOrderRequest request{};
  request.token = Token(sequence);
  request.client_order_id =
      MakeId<oms::api::ClientOrderId>("client-" + std::to_string(sequence));
  request.instrument_id = 1;
  request.side = oms::api::Side::Buy;
  request.type = oms::api::OrderType::Limit;
  request.time_in_force = oms::api::TimeInForce::GTC;
  request.flags = oms::api::PostOnly | oms::api::ReduceOnly |
                  oms::api::ClosePosition | oms::api::QuoteQuantity;
  request.quantity = {quantity, 3, {}};
  request.price = {5000, 4, {}};
  return request;
}

oms::api::SubmitOrderRequest Submitted(
    oms::api::NewOrderRequest request) {
  oms::api::SubmitOrderRequest submitted{};
  submitted.order = request;
  submitted.routing.kind = oms::api::ExecutionRouteKind::Polymarket;
  submitted.routing.venue =
      static_cast<std::uint8_t>(utils::md::Venue::Polymarket);
  submitted.routing.product_type =
      static_cast<std::uint8_t>(utils::md::ProductType::BinaryOption);
  submitted.routing.price_scale = 4;
  submitted.routing.quantity_scale = 3;
  submitted.routing.catalog_revision = 1;
  submitted.routing.tick_size = 1;
  submitted.routing.lot_size = 1;
  submitted.routing.minimum_order_size = 1;
  submitted.routing.signature_type = 3;
  submitted.routing.outcome = oms::api::PolymarketOutcome::Yes;
  submitted.routing.polymarket.condition_id[0] = 1;
  submitted.routing.polymarket.token_id[0] = 2;
  return submitted;
}

struct Updates {
  std::vector<oms::api::OrderUpdate> orders;
  std::vector<oms::api::FillUpdate> fills;

  Updates() {
    orders.reserve(128);
    fills.reserve(128);
  }

  static void OnOrder(void* context,
                      const oms::api::OrderUpdate& update) noexcept {
    static_cast<Updates*>(context)->orders.push_back(update);
  }
  static void OnFill(void* context,
                     const oms::api::FillUpdate& update) noexcept {
    static_cast<Updates*>(context)->fills.push_back(update);
  }
  oms::api::UpdateSink sink() {
    return {this, &Updates::OnOrder, &Updates::OnFill};
  }
};

struct Harness {
  oms::OrderTable table;
  oms::StateEngine engine;
  Updates updates;

  explicit Harness(std::size_t capacity = 32,
                   std::size_t dedup_capacity = 64)
      : table(capacity), engine(table, dedup_capacity) {}

  oms::api::OrderHandle Submit(std::uint64_t sequence,
                               std::int64_t quantity = 10) {
    oms::api::OrderHandle handle{};
    REQUIRE(engine.Submit(Submitted(Request(sequence, quantity)), handle,
                          updates.sink()) ==
            oms::api::Error::Ok);
    return handle;
  }
};

oms::api::VenueEvent Event(oms::api::VenueEventType type,
                           oms::api::OrderHandle handle) {
  oms::api::VenueEvent event{};
  event.type = type;
  event.handle = handle;
  return event;
}

oms::api::VenueEvent Fill(oms::api::OrderHandle handle, std::string_view trade,
                          std::int64_t quantity, std::int64_t price = 5000) {
  auto event = Event(oms::api::VenueEventType::Fill, handle);
  event.trade_id = MakeId<oms::api::TradeId>(trade);
  event.fill_quantity = {quantity, 3, {}};
  event.fill_price = {price, 4, {}};
  return event;
}

void TestExecutionDirectory() {
  oms::ExecutionDirectory directory(2);
  auto first = Submitted(Request(1)).routing;
  REQUIRE(directory.Register(1, first) == oms::api::Error::Ok);
  const auto* entry = directory.Find(1);
  REQUIRE(entry != nullptr);
  REQUIRE(entry->lifecycle == oms::ExecutionDirectory::Lifecycle::Active);
  REQUIRE(directory.Find(first) == 1);
  REQUIRE(directory.FindPolymarketToken(first.polymarket.token_id) == 1);
  REQUIRE(directory.Register(2, first) == oms::api::Error::Duplicate);

  auto second = first;
  second.polymarket.token_id[0] = 3;
  REQUIRE(directory.Register(2, second) == oms::api::Error::Ok);
  auto third = first;
  third.polymarket.token_id[0] = 4;
  REQUIRE(directory.Register(3, third) ==
          oms::api::Error::CapacityExceeded);

  REQUIRE(directory.Retire(1, true) == oms::api::Error::Deferred);
  entry = directory.Find(1);
  REQUIRE(entry != nullptr);
  REQUIRE(entry->lifecycle == oms::ExecutionDirectory::Lifecycle::Retiring);
  REQUIRE(directory.Retire(1, false) == oms::api::Error::Ok);
  REQUIRE(directory.Find(1) == nullptr);
}

void TestOrderTableIndexesCapacityAndAba() {
  oms::OrderTable table(2);
  oms::api::OrderHandle first{};
  oms::api::OrderHandle second{};
  REQUIRE(table.Insert(Submitted(Request(1)), first) == oms::api::Error::Ok);
  REQUIRE(table.Find(Token(1)) == table.Lookup(first));
  REQUIRE(table.Find(Request(1).client_order_id) == table.Lookup(first));
  const auto venue = MakeId<oms::api::VenueOrderId>("venue-1");
  REQUIRE(table.BindVenueId(first, venue) == oms::api::Error::Ok);
  REQUIRE(table.BindVenueId(first, venue) == oms::api::Error::Ok);
  REQUIRE(table.Find(venue) == table.Lookup(first));
  REQUIRE(table.BindVenueId(first, MakeId<oms::api::VenueOrderId>("other")) ==
          oms::api::Error::Conflict);

  REQUIRE(table.Insert(Submitted(Request(2)), second) == oms::api::Error::Ok);
  REQUIRE(table.BindVenueId(second, venue) == oms::api::Error::Conflict);
  oms::api::OrderHandle unused{};
  REQUIRE(table.Insert(Submitted(Request(3)), unused) ==
          oms::api::Error::CapacityExceeded);
  auto duplicate_token = Request(4);
  duplicate_token.token = Token(2);
  REQUIRE(table.Erase(first) == oms::api::Error::Ok);
  REQUIRE(table.Lookup(first) == nullptr);
  REQUIRE(table.Find(venue) == nullptr);
  REQUIRE(table.Insert(Submitted(Request(3)), unused) == oms::api::Error::Ok);
  REQUIRE(unused.slot == first.slot);
  REQUIRE(unused.generation != first.generation);
  REQUIRE(unused.generation != 0);
  REQUIRE(table.BindVenueId(first, venue) == oms::api::Error::StaleHandle);
  REQUIRE(table.Insert(Submitted(duplicate_token), first) ==
          oms::api::Error::Conflict);
}

void TestRoutingIsFrozenOnOrder() {
  oms::OrderTable table(1);
  oms::api::SubmitOrderRequest prepared{};
  prepared.order = Request(91);
  prepared.routing.kind = oms::api::ExecutionRouteKind::Polymarket;
  prepared.routing.venue =
      static_cast<std::uint8_t>(utils::md::Venue::Polymarket);
  prepared.routing.product_type =
      static_cast<std::uint8_t>(utils::md::ProductType::BinaryOption);
  prepared.routing.catalog_revision = 12;
  prepared.routing.price_scale = 2;
  prepared.routing.quantity_scale = 2;
  prepared.routing.tick_size = 1;
  prepared.routing.lot_size = 1;
  prepared.routing.minimum_order_size = 1;
  prepared.routing.signature_type = 3;
  prepared.routing.outcome = oms::api::PolymarketOutcome::Yes;
  prepared.routing.polymarket.condition_id[0] = 0x11;
  prepared.routing.polymarket.token_id[31] = 0x22;
  oms::api::OrderHandle handle{};
  REQUIRE(table.Insert(prepared, handle) == oms::api::Error::Ok);

  prepared.routing.catalog_revision = 13;
  prepared.routing.polymarket.condition_id[0] = 0x33;
  prepared.routing.polymarket.token_id[31] = 0x44;
  const auto* stored = table.Lookup(handle);
  REQUIRE(stored != nullptr);
  REQUIRE(stored->routing.catalog_revision == 12);
  REQUIRE(stored->routing.polymarket.condition_id[0] == 0x11);
  REQUIRE(stored->routing.polymarket.token_id[31] == 0x22);
}

void TestSubmitValidationAndLocalReject() {
  Harness harness;
  auto bad = Request(1);
  bad.quantity.scale = 2;
  oms::api::OrderHandle handle{};
  REQUIRE(harness.engine.Submit(Submitted(bad), handle,
                                harness.updates.sink()) ==
          oms::api::Error::InvalidScale);
  bad = Request(2);
  bad.time_in_force = oms::api::TimeInForce::GTD;
  REQUIRE(harness.engine.Submit(Submitted(bad), handle,
                                harness.updates.sink()) ==
          oms::api::Error::InvalidArgument);
  bad = Request(3);
  bad.type = oms::api::OrderType::Market;
  REQUIRE(harness.engine.Submit(Submitted(bad), handle,
                                harness.updates.sink()) ==
          oms::api::Error::InvalidArgument);
  bad = Request(4);
  bad.side = static_cast<oms::api::Side>(99);
  REQUIRE(harness.engine.Submit(Submitted(bad), handle,
                                harness.updates.sink()) ==
          oms::api::Error::InvalidArgument);
  bad = Request(5);
  bad.time_in_force = static_cast<oms::api::TimeInForce>(99);
  REQUIRE(harness.engine.Submit(Submitted(bad), handle,
                                harness.updates.sink()) ==
          oms::api::Error::InvalidArgument);
  bad = Request(6);
  bad.flags = static_cast<std::uint16_t>(1U << 15U);
  REQUIRE(harness.engine.Submit(Submitted(bad), handle,
                                harness.updates.sink()) ==
          oms::api::Error::InvalidArgument);

  handle = harness.Submit(7);
  REQUIRE(harness.updates.orders.size() == 1);
  REQUIRE(harness.updates.orders.back().type ==
          oms::api::UpdateType::Submitted);
  REQUIRE(harness.engine.RejectLocal(handle, harness.updates.sink()) ==
          oms::api::Error::Ok);
  REQUIRE(harness.table.Lookup(handle)->status ==
          oms::api::OrderStatus::Rejected);
  REQUIRE(harness.engine.RejectLocal(handle, harness.updates.sink()) ==
          oms::api::Error::InvalidTransition);
}

void TestAckCancelAndEarlyCancel() {
  Harness harness;
  auto handle = harness.Submit(1);
  REQUIRE(harness.engine.RequestCancel(Token(1), harness.updates.sink()) ==
          oms::api::Error::Ok);
  REQUIRE(harness.table.Lookup(handle)->inflight ==
          oms::api::InflightAction::Cancel);

  auto ack = Event(oms::api::VenueEventType::NewAck, handle);
  ack.venue_order_id = MakeId<oms::api::VenueOrderId>("venue-early");
  REQUIRE(harness.engine.Apply(ack, harness.updates.sink()) ==
          oms::api::Error::Ok);
  REQUIRE(harness.table.Lookup(handle)->status == oms::api::OrderStatus::Open);
  REQUIRE(harness.table.Lookup(handle)->inflight ==
          oms::api::InflightAction::Cancel);
  REQUIRE(harness.engine.Apply(
              Event(oms::api::VenueEventType::CancelReject, handle),
              harness.updates.sink()) == oms::api::Error::Ok);
  REQUIRE(harness.table.Lookup(handle)->inflight ==
          oms::api::InflightAction::None);
  REQUIRE(harness.engine.RequestCancel(Token(1), harness.updates.sink()) ==
          oms::api::Error::Ok);
  REQUIRE(harness.engine.Apply(
              Event(oms::api::VenueEventType::CancelAck, handle),
              harness.updates.sink()) == oms::api::Error::Ok);
  REQUIRE(harness.table.Lookup(handle)->status ==
          oms::api::OrderStatus::Canceled);
}

void TestFillsBeforeAckLateAndSeparate() {
  Harness harness;
  const auto handle = harness.Submit(1, 7);
  REQUIRE(harness.engine.Apply(Fill(handle, "t1", 1), harness.updates.sink()) ==
          oms::api::Error::Ok);
  REQUIRE(harness.table.Lookup(handle)->status ==
          oms::api::OrderStatus::PendingSubmit);
  REQUIRE(harness.engine.Apply(
              Event(oms::api::VenueEventType::NewAck, handle),
              harness.updates.sink()) == oms::api::Error::Ok);
  REQUIRE(harness.table.Lookup(handle)->status ==
          oms::api::OrderStatus::PartiallyFilled);
  for (int i = 2; i <= 5; ++i) {
    REQUIRE(harness.engine.Apply(
                Fill(handle, "t" + std::to_string(i), 1, 5000 + i),
                harness.updates.sink()) == oms::api::Error::Ok);
  }
  REQUIRE(harness.updates.fills.size() == 5);
  REQUIRE(harness.engine.RequestCancel(Token(1), harness.updates.sink()) ==
          oms::api::Error::Ok);
  REQUIRE(harness.engine.Apply(
              Event(oms::api::VenueEventType::CancelAck, handle),
              harness.updates.sink()) == oms::api::Error::Ok);
  REQUIRE(harness.engine.Apply(Fill(handle, "late", 1, 6000),
                               harness.updates.sink()) ==
          oms::api::Error::Ok);
  REQUIRE(harness.table.Lookup(handle)->status ==
          oms::api::OrderStatus::Canceled);
  REQUIRE(harness.updates.fills.size() == 6);
  REQUIRE(harness.updates.fills.back().remaining_quantity.value == 1);
}

void TestFillDuplicateConflictOverfillAndBounds() {
  Harness harness(8, 8);
  const auto handle = harness.Submit(1, 4);
  auto fill = Fill(handle, "same", 2, 7000);
  REQUIRE(harness.engine.Apply(fill, harness.updates.sink()) ==
          oms::api::Error::Ok);
  REQUIRE(harness.engine.Apply(fill, harness.updates.sink()) ==
          oms::api::Error::Ok);
  REQUIRE(harness.updates.fills.size() == 1);
  auto conflict = fill;
  conflict.fill_price.value = 7001;
  REQUIRE(harness.engine.Apply(conflict, harness.updates.sink()) ==
          oms::api::Error::ProtocolConflict);
  REQUIRE(harness.engine.Apply(Fill(handle, "over", 3),
                               harness.updates.sink()) ==
          oms::api::Error::Overfill);
  REQUIRE(harness.table.Lookup(handle)->cumulative_quantity == 2);
  auto wrong_scale = Fill(handle, "wrong-scale", 1);
  wrong_scale.fill_quantity.scale = 2;
  REQUIRE(harness.engine.Apply(wrong_scale, harness.updates.sink()) ==
          oms::api::Error::InvalidScale);

  Harness wide;
  auto request = Request(9, std::numeric_limits<std::int64_t>::max());
  request.price.value = std::numeric_limits<std::int64_t>::max();
  oms::api::OrderHandle wide_handle{};
  REQUIRE(wide.engine.Submit(Submitted(request), wide_handle,
                             wide.updates.sink()) ==
          oms::api::Error::Ok);
  REQUIRE(wide.engine.Apply(
              Fill(wide_handle, "wide", std::numeric_limits<std::int64_t>::max(),
                   std::numeric_limits<std::int64_t>::max()),
              wide.updates.sink()) == oms::api::Error::Ok);
  REQUIRE(wide.table.Lookup(wide_handle)->average_price ==
          std::numeric_limits<std::int64_t>::max());
}

void TestCorrelationAndIdempotentReports() {
  Harness harness;
  const auto first = harness.Submit(1, 4);
  const auto second = harness.Submit(2, 4);

  auto mismatch = Event(oms::api::VenueEventType::NewAck, first);
  mismatch.token = Token(2);
  REQUIRE(harness.engine.Apply(mismatch, harness.updates.sink()) ==
          oms::api::Error::ProtocolConflict);
  REQUIRE(harness.table.Lookup(first)->status ==
          oms::api::OrderStatus::PendingSubmit);
  REQUIRE(harness.table.Lookup(second)->status ==
          oms::api::OrderStatus::PendingSubmit);

  auto ack = Event(oms::api::VenueEventType::NewAck, first);
  ack.token = Token(1);
  ack.client_order_id = Request(1).client_order_id;
  ack.venue_order_id = MakeId<oms::api::VenueOrderId>(
      "0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef");
  REQUIRE(harness.engine.Apply(ack, harness.updates.sink()) ==
          oms::api::Error::Ok);
  const std::size_t updates_after_ack = harness.updates.orders.size();
  REQUIRE(harness.engine.Apply(ack, harness.updates.sink()) ==
          oms::api::Error::Ok);
  REQUIRE(harness.updates.orders.size() == updates_after_ack);

  auto venue_fill = Fill({}, "venue-only", 1, 3);
  venue_fill.venue_order_id = ack.venue_order_id;
  REQUIRE(harness.engine.Apply(venue_fill, harness.updates.sink()) ==
          oms::api::Error::Ok);
  REQUIRE(harness.table.Lookup(first)->cumulative_quantity == 1);

  auto client_expire = Event(oms::api::VenueEventType::Expire, {});
  client_expire.client_order_id = Request(2).client_order_id;
  REQUIRE(harness.engine.Apply(client_expire, harness.updates.sink()) ==
          oms::api::Error::Ok);
  const std::size_t updates_after_expire = harness.updates.orders.size();
  REQUIRE(harness.engine.Apply(client_expire, harness.updates.sink()) ==
          oms::api::Error::Ok);
  REQUIRE(harness.updates.orders.size() == updates_after_expire);

  Harness delayed;
  const auto delayed_handle = delayed.Submit(10);
  REQUIRE(delayed.engine.RequestCancel(Token(10), delayed.updates.sink()) ==
          oms::api::Error::Ok);
  REQUIRE(delayed.engine.Apply(
              Event(oms::api::VenueEventType::CancelAck, delayed_handle),
              delayed.updates.sink()) == oms::api::Error::Ok);
  auto late_ack = Event(oms::api::VenueEventType::NewAck, delayed_handle);
  late_ack.venue_order_id =
      MakeId<oms::api::VenueOrderId>("late-venue-id");
  REQUIRE(delayed.engine.Apply(late_ack, delayed.updates.sink()) ==
          oms::api::Error::Ok);
  REQUIRE(delayed.table.Lookup(delayed_handle)->status ==
          oms::api::OrderStatus::Canceled);
  REQUIRE(delayed.table.Find(late_ack.venue_order_id) ==
          delayed.table.Lookup(delayed_handle));
}

void TestExactAverageAndTerminalLateFills() {
  Harness average;
  const auto average_handle = average.Submit(1, 3);
  REQUIRE(average.engine.Apply(
              Event(oms::api::VenueEventType::NewAck, average_handle),
              average.updates.sink()) == oms::api::Error::Ok);
  REQUIRE(average.engine.Apply(Fill(average_handle, "avg-1", 1, 1),
                               average.updates.sink()) ==
          oms::api::Error::Ok);
  REQUIRE(average.engine.Apply(Fill(average_handle, "avg-2", 1, 2),
                               average.updates.sink()) ==
          oms::api::Error::Ok);
  REQUIRE(average.engine.Apply(Fill(average_handle, "avg-3", 1, 3),
                               average.updates.sink()) ==
          oms::api::Error::Ok);
  REQUIRE(average.table.Lookup(average_handle)->average_price == 2);

  Harness expired;
  const auto expired_handle = expired.Submit(2, 2);
  REQUIRE(expired.engine.Apply(
              Event(oms::api::VenueEventType::Expire, expired_handle),
              expired.updates.sink()) == oms::api::Error::Ok);
  REQUIRE(expired.engine.Apply(Fill(expired_handle, "late-expired", 1),
                               expired.updates.sink()) ==
          oms::api::Error::Ok);
  REQUIRE(expired.table.Lookup(expired_handle)->status ==
          oms::api::OrderStatus::Expired);

  Harness rejected;
  const auto rejected_handle = rejected.Submit(3, 2);
  REQUIRE(rejected.engine.RejectLocal(rejected_handle,
                                      rejected.updates.sink()) ==
          oms::api::Error::Ok);
  REQUIRE(rejected.engine.Apply(Fill(rejected_handle, "late-rejected", 1),
                                rejected.updates.sink()) ==
          oms::api::Error::Ok);
  REQUIRE(rejected.table.Lookup(rejected_handle)->status ==
          oms::api::OrderStatus::Rejected);
}

void TestRejectExpireAndReconcilePaths() {
  Harness harness;
  auto rejected = harness.Submit(1);
  REQUIRE(harness.engine.Apply(
              Event(oms::api::VenueEventType::NewReject, rejected),
              harness.updates.sink()) == oms::api::Error::Ok);
  REQUIRE(harness.engine.Apply(
              Event(oms::api::VenueEventType::NewAck, rejected),
              harness.updates.sink()) == oms::api::Error::ProtocolConflict);

  auto expired = harness.Submit(2);
  REQUIRE(harness.engine.Apply(
              Event(oms::api::VenueEventType::Expire, expired),
              harness.updates.sink()) == oms::api::Error::Ok);
  REQUIRE(harness.engine.Apply(
              Event(oms::api::VenueEventType::Expire, expired),
              harness.updates.sink()) == oms::api::Error::Ok);

  auto reconciled = harness.Submit(3);
  REQUIRE(harness.engine.Apply(
              Event(oms::api::VenueEventType::ReconcileOpen, reconciled),
              harness.updates.sink()) == oms::api::Error::Ok);
  REQUIRE(harness.table.Lookup(reconciled)->status ==
          oms::api::OrderStatus::Open);
  REQUIRE(harness.engine.Apply(
              [&] {
                auto event = Event(
                    oms::api::VenueEventType::ReconcileTerminal, reconciled);
                event.reconciled_status = oms::api::OrderStatus::Canceled;
                return event;
              }(),
              harness.updates.sink()) == oms::api::Error::Ok);
  REQUIRE(harness.table.Lookup(reconciled)->status ==
          oms::api::OrderStatus::Canceled);
}

void TestIllegalStatusInflightMatrix() {
  Harness harness(64);
  constexpr std::array statuses{
      oms::api::OrderStatus::PendingSubmit, oms::api::OrderStatus::Open,
      oms::api::OrderStatus::PartiallyFilled, oms::api::OrderStatus::Filled,
      oms::api::OrderStatus::Canceled, oms::api::OrderStatus::Rejected,
      oms::api::OrderStatus::Expired, oms::api::OrderStatus::Unknown};
  constexpr std::array inflights{
      oms::api::InflightAction::None, oms::api::InflightAction::Submit,
      oms::api::InflightAction::Cancel, oms::api::InflightAction::Reconcile};
  std::uint64_t sequence = 1;
  for (const auto status : statuses) {
    for (const auto inflight : inflights) {
      const auto handle = harness.Submit(sequence++);
      auto* record = harness.table.Lookup(handle);
      record->status = status;
      record->inflight = inflight;
      const bool terminal = status == oms::api::OrderStatus::Filled ||
                            status == oms::api::OrderStatus::Canceled ||
                            status == oms::api::OrderStatus::Rejected ||
                            status == oms::api::OrderStatus::Expired;
      const bool allowed =
          !terminal && inflight != oms::api::InflightAction::Cancel;
      const auto result =
          harness.engine.RequestCancel(record->request.token,
                                       harness.updates.sink());
      REQUIRE((result == oms::api::Error::Ok) == allowed);
    }
  }
}

void TestDeterministicRandomizedSequence() {
  Harness first(128, 256);
  Harness second(128, 256);
  std::uint64_t state = 0x5eed1234ULL;
  std::uint64_t sequence = 1;
  std::vector<oms::api::OrderHandle> first_handles;
  std::vector<oms::api::OrderHandle> second_handles;
  first_handles.reserve(64);
  second_handles.reserve(64);
  for (int step = 0; step < 200; ++step) {
    state ^= state << 13;
    state ^= state >> 7;
    state ^= state << 17;
    if ((state % 3 == 0 || first_handles.empty()) &&
        first_handles.size() < 64) {
      first_handles.push_back(first.Submit(sequence));
      second_handles.push_back(second.Submit(sequence));
      ++sequence;
      continue;
    }
    const std::size_t index =
        static_cast<std::size_t>(state % first_handles.size());
    auto* left = first.table.Lookup(first_handles[index]);
    auto* right = second.table.Lookup(second_handles[index]);
    if (left->status == oms::api::OrderStatus::PendingSubmit) {
      REQUIRE(first.engine.Apply(
                  Event(oms::api::VenueEventType::NewAck,
                        first_handles[index]),
                  first.updates.sink()) ==
              second.engine.Apply(
                  Event(oms::api::VenueEventType::NewAck,
                        second_handles[index]),
                  second.updates.sink()));
    } else if (left->status == oms::api::OrderStatus::Open &&
               left->remaining_quantity > 0) {
      const std::string trade = "r" + std::to_string(step);
      REQUIRE(first.engine.Apply(Fill(first_handles[index], trade, 1),
                                 first.updates.sink()) ==
              second.engine.Apply(Fill(second_handles[index], trade, 1),
                                  second.updates.sink()));
    }
    REQUIRE(left->status == right->status);
    REQUIRE(left->cumulative_quantity == right->cumulative_quantity);
    REQUIRE(left->remaining_quantity == right->remaining_quantity);
  }
  REQUIRE(first.updates.orders.size() == second.updates.orders.size());
  REQUIRE(first.updates.fills.size() == second.updates.fills.size());
  REQUIRE(first.updates.orders == second.updates.orders);
  REQUIRE(first.updates.fills == second.updates.fills);
}

void TestFillDedupCapacityAndFullBeforeAck() {
  Harness harness(8, 1);
  for (std::uint64_t sequence = 1; sequence <= 4; ++sequence) {
    const auto handle = harness.Submit(sequence, 1);
    REQUIRE(harness.engine.Apply(
                Fill(handle, "dedup-" + std::to_string(sequence), 1),
                harness.updates.sink()) == oms::api::Error::Ok);
  }
  const auto overflow = harness.Submit(5, 1);
  REQUIRE(harness.engine.Apply(Fill(overflow, "dedup-5", 1),
                               harness.updates.sink()) ==
          oms::api::Error::CapacityExceeded);

  Harness early;
  const auto handle = early.Submit(10, 1);
  REQUIRE(early.engine.RequestCancel(Token(10), early.updates.sink()) ==
          oms::api::Error::Ok);
  REQUIRE(early.engine.Apply(Fill(handle, "full-before-ack", 1),
                             early.updates.sink()) ==
          oms::api::Error::Ok);
  REQUIRE(early.engine.Apply(
              Event(oms::api::VenueEventType::NewAck, handle),
              early.updates.sink()) == oms::api::Error::Ok);
  REQUIRE(early.table.Lookup(handle)->status == oms::api::OrderStatus::Filled);
  REQUIRE(early.table.Lookup(handle)->inflight ==
          oms::api::InflightAction::None);
}

void TestHotPathDoesNotAllocate() {
  Harness harness;
  const auto request = Request(99, 2);
  auto ack = Event(oms::api::VenueEventType::NewAck, {});
  auto fill = Fill({}, "allocation-fill", 1);
  const oms::api::UpdateSink sink{};
  oms::api::OrderHandle handle{};

  tracked_allocations.store(0, std::memory_order_relaxed);
  track_allocations.store(true, std::memory_order_relaxed);
  const auto submit = harness.engine.Submit(Submitted(request), handle, sink);
  ack.handle = handle;
  fill.handle = handle;
  const auto accepted = harness.engine.Apply(ack, sink);
  const auto filled = harness.engine.Apply(fill, sink);
  const auto cancel = harness.engine.RequestCancel(request.token, sink);
  track_allocations.store(false, std::memory_order_relaxed);

  REQUIRE(submit == oms::api::Error::Ok);
  REQUIRE(accepted == oms::api::Error::Ok);
  REQUIRE(filled == oms::api::Error::Ok);
  REQUIRE(cancel == oms::api::Error::Ok);
  REQUIRE(tracked_allocations.load(std::memory_order_relaxed) == 0);
}

class JsonValidator {
 public:
  explicit JsonValidator(std::string_view input) : input_(input) {}
  bool valid() {
    Skip();
    if (!Value()) return false;
    Skip();
    return position_ == input_.size();
  }

 private:
  void Skip() {
    while (position_ < input_.size() &&
           (input_[position_] == ' ' || input_[position_] == '\n' ||
            input_[position_] == '\r' || input_[position_] == '\t'))
      ++position_;
  }
  bool Consume(char value) {
    Skip();
    if (position_ == input_.size() || input_[position_] != value) return false;
    ++position_;
    return true;
  }
  bool String() {
    if (!Consume('"')) return false;
    while (position_ < input_.size()) {
      const unsigned char value =
          static_cast<unsigned char>(input_[position_++]);
      if (value == '"') return true;
      if (value < 0x20U) return false;
      if (value != '\\') continue;
      if (position_ == input_.size()) return false;
      const char escaped = input_[position_++];
      if (std::string_view("\"\\/bfnrt").find(escaped) !=
          std::string_view::npos)
        continue;
      if (escaped != 'u' || position_ + 4 > input_.size()) return false;
      for (unsigned index = 0; index < 4; ++index) {
        const char digit = input_[position_++];
        if (!((digit >= '0' && digit <= '9') ||
              (digit >= 'a' && digit <= 'f') ||
              (digit >= 'A' && digit <= 'F')))
          return false;
      }
    }
    return false;
  }
  bool Number() {
    Skip();
    const std::size_t begin = position_;
    if (position_ < input_.size() && input_[position_] == '-') ++position_;
    if (position_ == input_.size()) return false;
    if (input_[position_] == '0') {
      ++position_;
    } else {
      if (input_[position_] < '1' || input_[position_] > '9') return false;
      while (position_ < input_.size() && input_[position_] >= '0' &&
             input_[position_] <= '9')
        ++position_;
    }
    if (position_ < input_.size() && input_[position_] == '.') {
      ++position_;
      const std::size_t digits = position_;
      while (position_ < input_.size() && input_[position_] >= '0' &&
             input_[position_] <= '9')
        ++position_;
      if (digits == position_) return false;
    }
    if (position_ < input_.size() &&
        (input_[position_] == 'e' || input_[position_] == 'E')) {
      ++position_;
      if (position_ < input_.size() &&
          (input_[position_] == '+' || input_[position_] == '-'))
        ++position_;
      const std::size_t digits = position_;
      while (position_ < input_.size() && input_[position_] >= '0' &&
             input_[position_] <= '9')
        ++position_;
      if (digits == position_) return false;
    }
    return position_ != begin;
  }
  bool Literal(std::string_view value) {
    Skip();
    if (input_.substr(position_, value.size()) != value) return false;
    position_ += value.size();
    return true;
  }
  bool Array() {
    if (!Consume('[')) return false;
    Skip();
    if (Consume(']')) return true;
    for (;;) {
      if (!Value()) return false;
      Skip();
      if (Consume(']')) return true;
      if (!Consume(',')) return false;
    }
  }
  bool Object() {
    if (!Consume('{')) return false;
    Skip();
    if (Consume('}')) return true;
    for (;;) {
      if (!String() || !Consume(':') || !Value()) return false;
      Skip();
      if (Consume('}')) return true;
      if (!Consume(',')) return false;
    }
  }
  bool Value() {
    Skip();
    if (position_ == input_.size()) return false;
    if (input_[position_] == '"') return String();
    if (input_[position_] == '{') return Object();
    if (input_[position_] == '[') return Array();
    if (input_[position_] == '-' ||
        (input_[position_] >= '0' && input_[position_] <= '9'))
      return Number();
    return Literal("true") || Literal("false") || Literal("null");
  }

  std::string_view input_;
  std::size_t position_{};
};

void TestFixtureManifestStructure() {
  const std::string path =
      std::string(OMS_TEST_FIXTURE_DIR) + "/manifest/fixture_manifest.json";
  std::ifstream input(path);
  REQUIRE(input.good());
  const std::string text((std::istreambuf_iterator<char>(input)),
                         std::istreambuf_iterator<char>());
  REQUIRE(JsonValidator(text).valid());
  REQUIRE(text.find("\"schema_version\": 1") != std::string::npos);
  REQUIRE(text.find("\"fixtures\"") != std::string::npos);
  REQUIRE(text.find("\"verification_tier\"") != std::string::npos);
  REQUIRE(text.find("\"binance.spot.place_order.ack.v1\"") !=
          std::string::npos);
  REQUIRE(text.find("\"polymarket.signing.v2_eip712.v1\"") !=
          std::string::npos);
  REQUIRE(text.find("\"path\": \"polymarket/signing_vectors.json\"") !=
          std::string::npos);

  std::size_t fixtures = 0;
  std::size_t cursor = 0;
  constexpr std::string_view path_marker = "\"path\": \"";
  while ((cursor = text.find(path_marker, cursor)) != std::string::npos) {
    cursor += path_marker.size();
    const std::size_t end = text.find('"', cursor);
    REQUIRE(end != std::string::npos);
    std::ifstream fixture(std::string(OMS_TEST_FIXTURE_DIR) + "/" +
                          text.substr(cursor, end - cursor));
    REQUIRE(fixture.good());
    const std::string fixture_text((std::istreambuf_iterator<char>(fixture)),
                                   std::istreambuf_iterator<char>());
    REQUIRE(JsonValidator(fixture_text).valid());
    ++fixtures;
    cursor = end + 1;
  }
  REQUIRE(fixtures == 20);
}

}  // namespace

int main() {
  const std::array tests{
      &TestExecutionDirectory,
      &TestOrderTableIndexesCapacityAndAba,
      &TestRoutingIsFrozenOnOrder,
      &TestSubmitValidationAndLocalReject,
      &TestAckCancelAndEarlyCancel,
      &TestFillsBeforeAckLateAndSeparate,
      &TestFillDuplicateConflictOverfillAndBounds,
      &TestCorrelationAndIdempotentReports,
      &TestExactAverageAndTerminalLateFills,
      &TestRejectExpireAndReconcilePaths,
      &TestIllegalStatusInflightMatrix,
      &TestDeterministicRandomizedSequence,
      &TestFillDedupCapacityAndFullBeforeAck,
      &TestHotPathDoesNotAllocate,
      &TestFixtureManifestStructure,
  };
  for (const auto test : tests) test();
  return 0;
}

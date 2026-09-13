#include "oms/execution_directory.h"

#include <algorithm>
#include <limits>
#include <stdexcept>

namespace oms {
namespace {

constexpr std::size_t kMissing = std::numeric_limits<std::size_t>::max();

std::size_t NextPowerOfTwo(std::size_t value) {
  if (value == 0 ||
      value > std::numeric_limits<std::uint32_t>::max() / 2U)
    throw std::invalid_argument("unsupported execution directory capacity");
  std::size_t result = 1;
  while (result < value * 2U) result *= 2U;
  return std::max<std::size_t>(4, result);
}

std::size_t Mix(const void* data, std::size_t size,
                std::size_t hash) noexcept {
  const auto* bytes = static_cast<const unsigned char*>(data);
  for (std::size_t index = 0; index < size; ++index) {
    hash ^= bytes[index];
    hash *= sizeof(std::size_t) == 8 ? 1099511628211ULL : 16777619U;
  }
  return hash;
}

std::size_t HashId(api::InstrumentId value) noexcept {
  return Mix(&value, sizeof(value),
             sizeof(std::size_t) == 8 ? 1469598103934665603ULL
                                      : 2166136261U);
}

std::size_t HashRoute(const api::ResolvedInstrument& value) noexcept {
  std::size_t hash = sizeof(std::size_t) == 8 ? 1469598103934665603ULL
                                              : 2166136261U;
  hash = Mix(&value.kind, sizeof(value.kind), hash);
  hash = Mix(&value.venue, sizeof(value.venue), hash);
  hash = Mix(&value.product_type, sizeof(value.product_type), hash);
  if (value.kind == api::ExecutionRouteKind::Crypto) {
    hash = Mix(value.crypto.venue_symbol.value.data(),
               value.crypto.venue_symbol.length, hash);
    return Mix(&value.crypto.venue_symbol.length,
               sizeof(value.crypto.venue_symbol.length), hash);
  }
  hash = Mix(value.polymarket.condition_id.data(),
             value.polymarket.condition_id.size(), hash);
  hash = Mix(value.polymarket.token_id.data(),
             value.polymarket.token_id.size(), hash);
  return Mix(&value.outcome, sizeof(value.outcome), hash);
}

bool SameRoute(const api::ResolvedInstrument& left,
               const api::ResolvedInstrument& right) noexcept {
  if (left.kind != right.kind || left.venue != right.venue ||
      left.product_type != right.product_type)
    return false;
  if (left.kind == api::ExecutionRouteKind::Crypto)
    return left.crypto.venue_symbol == right.crypto.venue_symbol;
  return left.outcome == right.outcome &&
         left.polymarket.condition_id == right.polymarket.condition_id &&
         left.polymarket.token_id == right.polymarket.token_id;
}

bool AnyByte(const std::array<std::uint8_t, 32>& value) noexcept {
  return std::any_of(value.begin(), value.end(),
                     [](std::uint8_t byte) { return byte != 0; });
}

api::ResolvedInstrument RouteKey(
    const api::VenueInstrumentRef& reference) noexcept {
  api::ResolvedInstrument key{};
  key.kind = reference.kind;
  key.venue = reference.venue;
  key.product_type = reference.product_type;
  key.outcome = reference.outcome;
  if (reference.kind == api::ExecutionRouteKind::Crypto) {
    key.crypto = reference.crypto;
  } else {
    key.polymarket.condition_id = reference.polymarket.condition_id;
    key.polymarket.token_id = reference.polymarket.token_id;
  }
  return key;
}

bool Valid(const api::ResolvedInstrument& value) noexcept {
  if (value.venue == 0 || value.product_type == 0 ||
      value.catalog_revision == 0 || value.price_scale > 18 ||
      value.quantity_scale > 18 || value.tick_size <= 0 ||
      value.lot_size <= 0)
    return false;
  if (value.kind == api::ExecutionRouteKind::Crypto)
    return value.crypto.venue_symbol.length != 0 &&
           value.crypto.venue_symbol.length <=
               value.crypto.venue_symbol.value.size();
  return value.kind == api::ExecutionRouteKind::Polymarket &&
         (value.outcome == api::PolymarketOutcome::Yes ||
          value.outcome == api::PolymarketOutcome::No) &&
         (value.signature_type == 0 || value.signature_type == 3) &&
         value.minimum_order_size > 0 &&
         AnyByte(value.polymarket.condition_id) &&
         AnyByte(value.polymarket.token_id);
}

}  // namespace

ExecutionDirectory::ExecutionDirectory(std::size_t capacity)
    : entries_(capacity),
      free_slots_(capacity),
      by_id_(NextPowerOfTwo(capacity)),
      by_route_(NextPowerOfTwo(capacity)),
      free_count_(capacity) {
  for (std::size_t index = 0; index < capacity; ++index)
    free_slots_[index] = static_cast<std::uint32_t>(capacity - index - 1U);
}

std::size_t ExecutionDirectory::FindIdSlot(
    api::InstrumentId instrument_id) const noexcept {
  const std::size_t mask = by_id_.size() - 1U;
  std::size_t index = HashId(instrument_id) & mask;
  for (std::size_t probe = 0; probe < by_id_.size(); ++probe) {
    const IdSlot& slot = by_id_[index];
    if (slot.state == 0) return kMissing;
    if (slot.state == 1 && slot.key == instrument_id) return index;
    index = (index + 1U) & mask;
  }
  return kMissing;
}

std::size_t ExecutionDirectory::FindRouteSlot(
    const api::ResolvedInstrument& routing) const noexcept {
  const std::size_t mask = by_route_.size() - 1U;
  std::size_t index = HashRoute(routing) & mask;
  for (std::size_t probe = 0; probe < by_route_.size(); ++probe) {
    const RouteSlot& slot = by_route_[index];
    if (slot.state == 0) return kMissing;
    if (slot.state == 1 && SameRoute(slot.key, routing)) return index;
    index = (index + 1U) & mask;
  }
  return kMissing;
}

const ExecutionDirectory::Entry* ExecutionDirectory::FindEntry(
    api::InstrumentId instrument_id) const noexcept {
  const std::size_t slot = FindIdSlot(instrument_id);
  return slot == kMissing ? nullptr : &entries_[by_id_[slot].entry];
}

api::Error ExecutionDirectory::InsertId(api::InstrumentId instrument_id,
                                        std::uint32_t entry) noexcept {
  const std::size_t mask = by_id_.size() - 1U;
  std::size_t index = HashId(instrument_id) & mask;
  std::size_t deleted = kMissing;
  for (std::size_t probe = 0; probe < by_id_.size(); ++probe) {
    IdSlot& slot = by_id_[index];
    if (slot.state == 2 && deleted == kMissing) deleted = index;
    if (slot.state == 0) {
      by_id_[deleted == kMissing ? index : deleted] =
          {instrument_id, entry, 1};
      return api::Error::Ok;
    }
    index = (index + 1U) & mask;
  }
  if (deleted != kMissing) {
    by_id_[deleted] = {instrument_id, entry, 1};
    return api::Error::Ok;
  }
  return api::Error::CapacityExceeded;
}

api::Error ExecutionDirectory::InsertRoute(
    const api::ResolvedInstrument& routing, std::uint32_t entry) noexcept {
  const std::size_t mask = by_route_.size() - 1U;
  std::size_t index = HashRoute(routing) & mask;
  std::size_t deleted = kMissing;
  for (std::size_t probe = 0; probe < by_route_.size(); ++probe) {
    RouteSlot& slot = by_route_[index];
    if (slot.state == 2 && deleted == kMissing) deleted = index;
    if (slot.state == 0) {
      by_route_[deleted == kMissing ? index : deleted] = {routing, entry, 1};
      return api::Error::Ok;
    }
    index = (index + 1U) & mask;
  }
  if (deleted != kMissing) {
    by_route_[deleted] = {routing, entry, 1};
    return api::Error::Ok;
  }
  return api::Error::CapacityExceeded;
}

void ExecutionDirectory::EraseId(api::InstrumentId instrument_id) noexcept {
  const std::size_t slot = FindIdSlot(instrument_id);
  if (slot != kMissing) by_id_[slot].state = 2;
}

void ExecutionDirectory::EraseRoute(
    const api::ResolvedInstrument& routing) noexcept {
  const std::size_t slot = FindRouteSlot(routing);
  if (slot != kMissing) by_route_[slot].state = 2;
}

api::Error ExecutionDirectory::Register(
    api::InstrumentId instrument_id,
    const api::ResolvedInstrument& routing) noexcept {
  ++access_count_;
  if (instrument_id == 0 || !Valid(routing))
    return api::Error::InvalidArgument;
  if (const Entry* existing = FindEntry(instrument_id); existing != nullptr)
    return existing->lifecycle == Lifecycle::Active &&
                   SameRoute(existing->routing, routing)
               ? api::Error::Duplicate
               : api::Error::Conflict;
  if (FindRouteSlot(routing) != kMissing) return api::Error::Duplicate;
  if (free_count_ == 0) return api::Error::CapacityExceeded;

  const std::uint32_t entry_index = free_slots_[--free_count_];
  entries_[entry_index] = {instrument_id, routing, Lifecycle::Active, true};
  const api::Error id_result = InsertId(instrument_id, entry_index);
  const api::Error route_result = InsertRoute(routing, entry_index);
  if (id_result != api::Error::Ok || route_result != api::Error::Ok) {
    EraseId(instrument_id);
    EraseRoute(routing);
    entries_[entry_index] = {};
    free_slots_[free_count_++] = entry_index;
    return api::Error::CapacityExceeded;
  }
  ++size_;
  return api::Error::Ok;
}

api::Error ExecutionDirectory::Retire(
    api::InstrumentId instrument_id,
    bool has_active_or_inflight) noexcept {
  ++access_count_;
  const std::size_t id_slot = FindIdSlot(instrument_id);
  if (id_slot == kMissing) return api::Error::NotFound;
  const std::uint32_t entry_index = by_id_[id_slot].entry;
  Entry& entry = entries_[entry_index];
  entry.lifecycle = Lifecycle::Retiring;
  if (has_active_or_inflight) return api::Error::Deferred;
  EraseRoute(entry.routing);
  by_id_[id_slot].state = 2;
  entry = {};
  free_slots_[free_count_++] = entry_index;
  --size_;
  return api::Error::Ok;
}

const ExecutionDirectory::Entry* ExecutionDirectory::Find(
    api::InstrumentId instrument_id) const noexcept {
  ++access_count_;
  return FindEntry(instrument_id);
}

api::InstrumentId ExecutionDirectory::Find(
    const api::ResolvedInstrument& routing) const noexcept {
  ++access_count_;
  const std::size_t slot = FindRouteSlot(routing);
  return slot == kMissing ? 0 : entries_[by_route_[slot].entry].instrument_id;
}

api::InstrumentId ExecutionDirectory::Find(
    const api::VenueInstrumentRef& reference) const noexcept {
  return Find(RouteKey(reference));
}

api::InstrumentId ExecutionDirectory::FindPolymarketToken(
    const std::array<std::uint8_t, 32>& token_id) const noexcept {
  ++access_count_;
  for (const Entry& entry : entries_) {
    if (entry.occupied &&
        entry.routing.kind == api::ExecutionRouteKind::Polymarket &&
        entry.routing.polymarket.token_id == token_id)
      return entry.instrument_id;
  }
  return 0;
}

}  // namespace oms

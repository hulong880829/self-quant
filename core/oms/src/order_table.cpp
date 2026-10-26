#include "oms/order_table.h"

#include <algorithm>
#include <limits>
#include <stdexcept>
#include <type_traits>
#include <utility>

namespace oms {
namespace {

constexpr std::size_t NextPowerOfTwo(std::size_t value) noexcept {
  std::size_t result = 1;
  while (result < value && result <= std::numeric_limits<std::size_t>::max() / 2)
    result *= 2;
  return result;
}

std::size_t CheckedCapacity(std::size_t capacity) {
  if (capacity == 0 ||
      capacity > std::numeric_limits<std::uint32_t>::max() ||
      capacity > std::numeric_limits<std::size_t>::max() / 2) {
    throw std::invalid_argument("unsupported OMS order capacity");
  }
  return capacity;
}

std::size_t HashMix(const unsigned char* bytes, std::size_t size,
                    std::size_t hash) noexcept {
  for (std::size_t i = 0; i < size; ++i) {
    hash ^= bytes[i];
    hash *= sizeof(std::size_t) == 8 ? 1099511628211ULL : 16777619U;
  }
  return hash;
}

std::size_t HashKey(const api::RequestToken& value) noexcept {
  std::size_t hash = sizeof(std::size_t) == 8 ? 1469598103934665603ULL
                                               : 2166136261U;
  hash = HashMix(reinterpret_cast<const unsigned char*>(&value.lane),
                 sizeof(value.lane), hash);
  hash = HashMix(
      reinterpret_cast<const unsigned char*>(&value.session_epoch),
      sizeof(value.session_epoch), hash);
  hash = HashMix(reinterpret_cast<const unsigned char*>(&value.sequence),
                 sizeof(value.sequence), hash);
  return hash;
}

template <std::size_t Capacity, typename Tag>
std::size_t HashKey(const api::FixedId<Capacity, Tag>& value) noexcept {
  std::size_t hash = sizeof(std::size_t) == 8 ? 1469598103934665603ULL
                                               : 2166136261U;
  hash = HashMix(reinterpret_cast<const unsigned char*>(value.value.data()),
                 value.length, hash);
  return HashMix(reinterpret_cast<const unsigned char*>(&value.length),
                 sizeof(value.length), hash);
}

template <std::size_t Capacity, typename Tag>
bool ValidId(const api::FixedId<Capacity, Tag>& id) noexcept {
  return id.length > 0 && id.length <= Capacity;
}

template <typename Key>
class FixedMap {
 public:
  explicit FixedMap(std::size_t capacity)
      : entries_(NextPowerOfTwo(std::max<std::size_t>(4, capacity * 2))) {}

  api::OrderHandle* Find(const Key& key) noexcept {
    const std::size_t mask = entries_.size() - 1;
    std::size_t index = HashKey(key) & mask;
    for (std::size_t probe = 0; probe < entries_.size(); ++probe) {
      Entry& entry = entries_[index];
      if (entry.state == 0) return nullptr;
      if (entry.state == 1 && entry.key == key) return &entry.value;
      index = (index + 1) & mask;
    }
    return nullptr;
  }

  const api::OrderHandle* Find(const Key& key) const noexcept {
    return const_cast<FixedMap*>(this)->Find(key);
  }

  api::Error Insert(const Key& key, api::OrderHandle value) noexcept {
    if (const auto* existing = Find(key); existing != nullptr)
      return *existing == value ? api::Error::Ok : api::Error::Conflict;
    const std::size_t mask = entries_.size() - 1;
    std::size_t index = HashKey(key) & mask;
    std::size_t deleted = entries_.size();
    for (std::size_t probe = 0; probe < entries_.size(); ++probe) {
      Entry& entry = entries_[index];
      if (entry.state == 2 && deleted == entries_.size()) deleted = index;
      if (entry.state == 0) {
        Entry& target =
            entries_[deleted == entries_.size() ? index : deleted];
        target.key = key;
        target.value = value;
        target.state = 1;
        return api::Error::Ok;
      }
      index = (index + 1) & mask;
    }
    if (deleted != entries_.size()) {
      entries_[deleted] = Entry{key, value, 1};
      return api::Error::Ok;
    }
    return api::Error::CapacityExceeded;
  }

  void Erase(const Key& key) noexcept {
    const std::size_t mask = entries_.size() - 1;
    std::size_t index = HashKey(key) & mask;
    for (std::size_t probe = 0; probe < entries_.size(); ++probe) {
      Entry& entry = entries_[index];
      if (entry.state == 0) return;
      if (entry.state == 1 && entry.key == key) {
        entry.state = 2;
        return;
      }
      index = (index + 1) & mask;
    }
  }

 private:
  struct Entry {
    Key key{};
    api::OrderHandle value{};
    std::uint8_t state{};
  };
  std::vector<Entry> entries_;
};

}  // namespace

struct OrderTable::Impl {
  explicit Impl(std::size_t capacity)
      : by_token(capacity), by_client(capacity), by_venue(capacity) {}
  FixedMap<api::RequestToken> by_token;
  FixedMap<api::ClientOrderId> by_client;
  FixedMap<api::VenueOrderId> by_venue;
};

OrderTable::OrderTable(std::size_t capacity)
    : records_(CheckedCapacity(capacity)),
      free_slots_(CheckedCapacity(capacity)),
      free_count_(CheckedCapacity(capacity)),
      impl_(new Impl(CheckedCapacity(capacity))) {
  for (std::size_t i = 0; i < capacity; ++i)
    free_slots_[i] = static_cast<std::uint32_t>(capacity - i - 1);
}

OrderTable::~OrderTable() { delete impl_; }

api::Error OrderTable::Insert(const api::SubmitOrderRequest& submitted,
                              api::OrderHandle& handle) noexcept {
  const auto& request = submitted.order;
  if (request.token.sequence == 0 || !ValidId(request.client_order_id) ||
      request.quantity.value <= 0)
    return api::Error::InvalidArgument;
  if (impl_->by_token.Find(request.token) != nullptr ||
      impl_->by_client.Find(request.client_order_id) != nullptr)
    return api::Error::Conflict;
  if (free_count_ == 0 && ReclaimOldestTerminal() != api::Error::Ok)
    return api::Error::CapacityExceeded;

  const std::uint32_t slot = free_slots_[--free_count_];
  OrderRecord& record = records_[slot];
  std::uint64_t generation = record.generation + 1;
  if (generation == 0) generation = 1;
  record = {};
  record.terminal_prev = 0xffffffffu;
  record.terminal_next = 0xffffffffu;
  record.in_terminal = false;
  record.request = request;
  record.routing = submitted.routing;
  record.status = api::OrderStatus::PendingSubmit;
  record.inflight = api::InflightAction::Submit;
  record.remaining_quantity = request.quantity.value;
  record.generation = generation;
  record.occupied = true;
  handle = {slot, 0, generation};

  const api::Error token_result = impl_->by_token.Insert(request.token, handle);
  const api::Error client_result =
      impl_->by_client.Insert(request.client_order_id, handle);
  if (token_result != api::Error::Ok || client_result != api::Error::Ok) {
    impl_->by_token.Erase(request.token);
    impl_->by_client.Erase(request.client_order_id);
    record.occupied = false;
    record.in_terminal = false;
    record.terminal_prev = 0xffffffffu;
    record.terminal_next = 0xffffffffu;
    free_slots_[free_count_++] = slot;
    return api::Error::CapacityExceeded;
  }
  ++size_;
  return api::Error::Ok;
}

OrderRecord* OrderTable::Lookup(api::OrderHandle handle) noexcept {
  if (handle.generation == 0 || handle.reserved != 0 ||
      handle.slot >= records_.size())
    return nullptr;
  OrderRecord& record = records_[handle.slot];
  return record.occupied && record.generation == handle.generation ? &record
                                                                   : nullptr;
}

const OrderRecord* OrderTable::Lookup(api::OrderHandle handle) const noexcept {
  return const_cast<OrderTable*>(this)->Lookup(handle);
}

OrderRecord* OrderTable::Find(api::RequestToken token) noexcept {
  const auto* handle = impl_->by_token.Find(token);
  return handle == nullptr ? nullptr : Lookup(*handle);
}

OrderRecord* OrderTable::Find(api::ClientOrderId client_id) noexcept {
  if (!ValidId(client_id)) return nullptr;
  const auto* handle = impl_->by_client.Find(client_id);
  return handle == nullptr ? nullptr : Lookup(*handle);
}

OrderRecord* OrderTable::Find(api::VenueOrderId venue_id) noexcept {
  if (!ValidId(venue_id)) return nullptr;
  const auto* handle = impl_->by_venue.Find(venue_id);
  return handle == nullptr ? nullptr : Lookup(*handle);
}

bool OrderTable::IsTerminalStatus(api::OrderStatus status) noexcept {
  return status == api::OrderStatus::Filled ||
         status == api::OrderStatus::Canceled ||
         status == api::OrderStatus::Rejected ||
         status == api::OrderStatus::Expired;
}

void OrderTable::UnlinkTerminal(std::uint32_t slot) noexcept {
  OrderRecord& record = records_[slot];
  if (!record.in_terminal) return;
  constexpr std::uint32_t kMissing = 0xffffffffu;
  const std::uint32_t prev = record.terminal_prev;
  const std::uint32_t next = record.terminal_next;
  const bool prev_ok = prev < records_.size() && records_[prev].occupied &&
                       records_[prev].in_terminal &&
                       records_[prev].terminal_next == slot;
  const bool next_ok = next < records_.size() && records_[next].occupied &&
                       records_[next].in_terminal &&
                       records_[next].terminal_prev == slot;
  if (prev_ok) records_[prev].terminal_next = next_ok ? next : kMissing;
  if (next_ok) records_[next].terminal_prev = prev_ok ? prev : kMissing;
  if (terminal_head_.slot == slot &&
      terminal_head_.generation == record.generation) {
    terminal_head_ = next_ok ? TerminalLink{next, records_[next].generation}
                             : TerminalLink{};
  }
  if (terminal_tail_.slot == slot &&
      terminal_tail_.generation == record.generation) {
    terminal_tail_ = prev_ok ? TerminalLink{prev, records_[prev].generation}
                             : TerminalLink{};
  }
  record.in_terminal = false;
  record.terminal_prev = kMissing;
  record.terminal_next = kMissing;
}

api::Error OrderTable::ReclaimOldestTerminal() noexcept {
  constexpr std::uint32_t kMissing = 0xffffffffu;
  if (terminal_head_.slot == kMissing ||
      terminal_head_.slot >= records_.size())
    return api::Error::CapacityExceeded;
  OrderRecord& record = records_[terminal_head_.slot];
  if (!record.occupied || !record.in_terminal ||
      record.generation != terminal_head_.generation ||
      !IsTerminalStatus(record.status) ||
      record.inflight != api::InflightAction::None)
    return api::Error::CapacityExceeded;
  return Erase({terminal_head_.slot, 0, record.generation});
}

api::Error OrderTable::MarkTerminal(api::OrderHandle handle) noexcept {
  constexpr std::uint32_t kMissing = 0xffffffffu;
  OrderRecord* record = Lookup(handle);
  if (record == nullptr) return api::Error::StaleHandle;
  if (!IsTerminalStatus(record->status)) return api::Error::InvalidTransition;
  if (record->in_terminal) return api::Error::Ok;
  record->terminal_prev = kMissing;
  record->terminal_next = kMissing;
  if (terminal_tail_.slot < records_.size()) {
    OrderRecord& previous = records_[terminal_tail_.slot];
    if (!previous.occupied || !previous.in_terminal ||
        previous.generation != terminal_tail_.generation)
      return api::Error::InvalidTransition;
    previous.terminal_next = handle.slot;
    record->terminal_prev = terminal_tail_.slot;
  } else {
    terminal_head_ = {handle.slot, handle.generation};
  }
  record->in_terminal = true;
  terminal_tail_ = {handle.slot, handle.generation};
  if (terminal_head_.slot >= records_.size()) terminal_head_ = terminal_tail_;
  return api::Error::Ok;
}

api::OrderHandle OrderTable::HandleOf(const OrderRecord& record) const noexcept {
  const auto* begin = records_.data();
  const auto* pointer = &record;
  if (pointer < begin || pointer >= begin + records_.size()) return {};
  return {static_cast<std::uint32_t>(pointer - begin), 0, record.generation};
}

api::OrderHandle OrderTable::FindNonTerminal(
    api::InstrumentId instrument_id) const noexcept {
  for (const OrderRecord& record : records_) {
    if (!record.occupied || record.request.instrument_id != instrument_id)
      continue;
    if (!IsTerminalStatus(record.status)) return HandleOf(record);
  }
  return {};
}

bool OrderTable::HasActiveOrInflight(
    api::InstrumentId instrument_id) const noexcept {
  for (const OrderRecord& record : records_) {
    if (!record.occupied || record.request.instrument_id != instrument_id)
      continue;
    const bool terminal =
        record.status == api::OrderStatus::Filled ||
        record.status == api::OrderStatus::Canceled ||
        record.status == api::OrderStatus::Rejected ||
        record.status == api::OrderStatus::Expired;
    if (!terminal || record.inflight != api::InflightAction::None) return true;
  }
  return false;
}

api::Error OrderTable::BindVenueId(api::OrderHandle handle,
                                   api::VenueOrderId venue_id) noexcept {
  OrderRecord* record = Lookup(handle);
  if (record == nullptr) return api::Error::StaleHandle;
  if (!ValidId(venue_id)) return api::Error::InvalidArgument;
  if (record->venue_order_id.length != 0)
    return record->venue_order_id == venue_id ? api::Error::Ok
                                               : api::Error::Conflict;
  const api::Error result = impl_->by_venue.Insert(venue_id, handle);
  if (result == api::Error::Ok) record->venue_order_id = venue_id;
  return result;
}

api::Error OrderTable::Erase(api::OrderHandle handle) noexcept {
  OrderRecord* record = Lookup(handle);
  if (record == nullptr) return api::Error::StaleHandle;
  UnlinkTerminal(handle.slot);
  impl_->by_token.Erase(record->request.token);
  impl_->by_client.Erase(record->request.client_order_id);
  if (record->venue_order_id.length != 0)
    impl_->by_venue.Erase(record->venue_order_id);
  record->occupied = false;
  record->in_terminal = false;
  record->terminal_prev = 0xffffffffu;
  record->terminal_next = 0xffffffffu;
  free_slots_[free_count_++] = handle.slot;
  --size_;
  return api::Error::Ok;
}

api::Error OrderTable::EraseTerminalByInstrument(
    api::InstrumentId instrument_id) noexcept {
  if (instrument_id == 0) return api::Error::InvalidArgument;
  for (const OrderRecord& record : records_) {
    if (!record.occupied || record.request.instrument_id != instrument_id)
      continue;
    if (!IsTerminalStatus(record.status) ||
        record.inflight != api::InflightAction::None)
      return api::Error::InconsistentState;
  }
  for (std::size_t slot = 0; slot < records_.size(); ++slot) {
    OrderRecord& record = records_[slot];
    if (!record.occupied || record.request.instrument_id != instrument_id)
      continue;
    RememberRetiredToken(record.request.token);
    const api::Error erased =
        Erase({static_cast<std::uint32_t>(slot), 0, record.generation});
    if (erased != api::Error::Ok) return erased;
  }
  return api::Error::Ok;
}

void OrderTable::RememberRetiredToken(api::RequestToken token) noexcept {
  if (token.sequence == 0) return;
  retired_tokens_[retired_token_next_ % retired_tokens_.size()] = token;
  if (retired_token_count_ < retired_tokens_.size()) ++retired_token_count_;
  ++retired_token_next_;
}

bool OrderTable::WasRetiredToken(api::RequestToken token) const noexcept {
  if (token.sequence == 0) return false;
  for (std::uint32_t index = 0; index < retired_token_count_; ++index) {
    if (retired_tokens_[index] == token) return true;
  }
  return false;
}

}  // namespace oms

#include "strategyframe/managers.h"

#include <algorithm>
#include <array>
#include <cstring>
#include <limits>
#include <vector>

namespace strategyframe {
namespace {

inline constexpr std::uint32_t kEmpty = std::numeric_limits<std::uint32_t>::max();
inline constexpr std::uint32_t kTombstone = kEmpty - 1U;

std::uint64_t TokenHash(const OrderToken& token) noexcept {
  std::uint64_t value = token.sequence;
  value ^= static_cast<std::uint64_t>(token.lane) << 32U;
  value ^= token.session_epoch;
  value ^= value >> 30U;
  value *= 0xbf58476d1ce4e5b9ULL;
  value ^= value >> 27U;
  value *= 0x94d049bb133111ebULL;
  return value ^ (value >> 31U);
}

bool IsPowerOfTwo(std::size_t value) noexcept {
  return value >= 2 && (value & (value - 1U)) == 0;
}

struct FixedText {
  std::array<char, 96> data{};
  std::uint16_t size{};

  void assign(std::string_view source) noexcept {
    size = static_cast<std::uint16_t>(
        std::min(source.size(), data.size()));
    if (size != 0) std::memcpy(data.data(), source.data(), size);
  }
  [[nodiscard]] std::string_view view() const noexcept {
    return {data.data(), size};
  }
};

std::uint64_t PositionHash(AccountId account, InstrumentId instrument,
                           PositionSide side) noexcept {
  std::uint64_t value = static_cast<std::uint64_t>(account) << 32U;
  value ^= instrument;
  value ^= static_cast<std::uint64_t>(side) << 60U;
  value ^= value >> 33U;
  value *= 0xff51afd7ed558ccdULL;
  return value ^ (value >> 33U);
}

std::uint64_t TradeHash(AccountId account, const TradeId& trade) noexcept {
  std::uint64_t value =
      1469598103934665603ULL ^ static_cast<std::uint64_t>(account);
  const std::size_t length =
      std::min<std::size_t>(trade.length, sizeof(trade.value));
  for (std::size_t index = 0; index < length; ++index) {
    value ^= static_cast<unsigned char>(trade.value[index]);
    value *= 1099511628211ULL;
  }
  return value;
}

bool Rescale(std::int64_t value, std::uint8_t from, std::uint8_t to,
             std::int64_t& output) noexcept {
  while (from < to) {
    if (value > std::numeric_limits<std::int64_t>::max() / 10 ||
        value < std::numeric_limits<std::int64_t>::min() / 10)
      return false;
    value *= 10;
    ++from;
  }
  while (from > to) {
    if (value % 10 != 0) return false;
    value /= 10;
    --from;
  }
  output = value;
  return true;
}

bool Adjust(std::int64_t current, std::int64_t amount, bool add,
            std::int64_t& output) noexcept {
  if (add) {
    if ((amount > 0 &&
         current > std::numeric_limits<std::int64_t>::max() - amount) ||
        (amount < 0 &&
         current < std::numeric_limits<std::int64_t>::min() - amount))
      return false;
    output = current + amount;
    return true;
  }
  if ((amount > 0 &&
       current < std::numeric_limits<std::int64_t>::min() + amount) ||
      (amount < 0 &&
       current > std::numeric_limits<std::int64_t>::max() + amount))
    return false;
  output = current - amount;
  return true;
}

}  // namespace

struct OrderManager::Impl {
  struct Storage {
    FixedText client_id{};
    FixedText venue_id{};
  };

  explicit Impl(std::size_t requested)
      : capacity(IsPowerOfTwo(requested) ? requested : 2),
        mask(capacity - 1U),
        views(capacity),
        storage(capacity),
        index(capacity, kEmpty) {}

  [[nodiscard]] std::uint32_t find_index(OrderToken token) const noexcept {
    std::size_t slot = static_cast<std::size_t>(TokenHash(token)) & mask;
    for (std::size_t probe = 0; probe < capacity; ++probe) {
      const std::uint32_t value = index[slot];
      if (value == kEmpty) return kEmpty;
      if (value != kTombstone && value < count &&
          views[value].token == token)
        return value;
      slot = (slot + 1U) & mask;
    }
    return kEmpty;
  }

  [[nodiscard]] bool add_index(OrderToken token,
                               std::uint32_t dense) noexcept {
    std::size_t slot = static_cast<std::size_t>(TokenHash(token)) & mask;
    std::size_t first_tombstone = capacity;
    for (std::size_t probe = 0; probe < capacity; ++probe) {
      if (index[slot] == kEmpty) {
        index[first_tombstone == capacity ? slot : first_tombstone] = dense;
        return true;
      }
      if (index[slot] == kTombstone && first_tombstone == capacity)
        first_tombstone = slot;
      slot = (slot + 1U) & mask;
    }
    if (first_tombstone != capacity) {
      index[first_tombstone] = dense;
      return true;
    }
    return false;
  }

  void remove_index(OrderToken token) noexcept {
    std::size_t slot = static_cast<std::size_t>(TokenHash(token)) & mask;
    for (std::size_t probe = 0; probe < capacity; ++probe) {
      if (index[slot] == kEmpty) return;
      const std::uint32_t value = index[slot];
      if (value != kTombstone && value < count &&
          views[value].token == token) {
        index[slot] = kTombstone;
        return;
      }
      slot = (slot + 1U) & mask;
    }
  }

  void refresh_views(std::uint32_t dense) noexcept {
    views[dense].client_order_id = storage[dense].client_id.view();
    views[dense].venue_order_id = storage[dense].venue_id.view();
  }

  void update_dense_index(OrderToken token, std::uint32_t dense) noexcept {
    std::size_t slot = static_cast<std::size_t>(TokenHash(token)) & mask;
    for (std::size_t probe = 0; probe < capacity; ++probe) {
      if (index[slot] == kEmpty) return;
      const std::uint32_t value = index[slot];
      if (value != kTombstone && value < count &&
          views[value].token == token) {
        index[slot] = dense;
        return;
      }
      slot = (slot + 1U) & mask;
    }
  }

  void erase(std::uint32_t dense) noexcept {
    const OrderToken erased = views[dense].token;
    remove_index(erased);
    const std::uint32_t last = static_cast<std::uint32_t>(count - 1U);
    if (dense != last) {
      views[dense] = views[last];
      storage[dense] = storage[last];
      refresh_views(dense);
      update_dense_index(views[dense].token, dense);
    }
    views[last] = {};
    storage[last] = {};
    --count;
  }

  std::size_t capacity;
  std::size_t mask;
  std::size_t count{};
  std::vector<OrderView> views;
  std::vector<Storage> storage;
  std::vector<std::uint32_t> index;
};

OrderManager::OrderManager(std::size_t capacity)
    : impl_(std::make_unique<Impl>(capacity)) {}
OrderManager::~OrderManager() = default;
OrderManager::OrderManager(OrderManager&&) noexcept = default;
OrderManager& OrderManager::operator=(OrderManager&&) noexcept = default;

std::span<const OrderView> OrderManager::open_orders() const noexcept {
  return {impl_->views.data(), impl_->count};
}

Result<OrderView> OrderManager::find(OrderToken token) const noexcept {
  const std::uint32_t index = impl_->find_index(token);
  if (index == kEmpty) return {{}, Error::NotFound};
  return {impl_->views[index], Error::Ok};
}

std::size_t OrderManager::size() const noexcept { return impl_->count; }
std::size_t OrderManager::capacity() const noexcept {
  return impl_->capacity;
}

Error OrderManager::insert_pending(const OrderRequest& request,
                                   OrderToken token) noexcept {
  if (impl_->count == impl_->capacity) return Error::CapacityExceeded;
  if (impl_->find_index(token) != kEmpty) return Error::InvalidState;
  const auto dense = static_cast<std::uint32_t>(impl_->count);
  OrderView view;
  view.account_id = request.account_id;
  view.instrument_id = request.instrument_id;
  view.token = token;
  view.status = OrderStatus::PendingSubmit;
  view.side = request.side;
  view.type = request.type;
  view.time_in_force = request.time_in_force;
  view.quantity = request.quantity;
  view.price = request.price;
  view.remaining_quantity = request.quantity;
  impl_->views[dense] = view;
  impl_->storage[dense].client_id.assign(request.client_order_id);
  impl_->refresh_views(dense);
  if (!impl_->add_index(token, dense)) {
    impl_->views[dense] = {};
    impl_->storage[dense] = {};
    return Error::CapacityExceeded;
  }
  ++impl_->count;
  return Error::Ok;
}

Error OrderManager::reconcile_open(const OrderView& order) noexcept {
  const std::uint32_t existing = impl_->find_index(order.token);
  if (existing != kEmpty) {
    OrderView& current = impl_->views[existing];
    if (current.status == OrderStatus::Filled ||
        current.status == OrderStatus::Canceled ||
        current.status == OrderStatus::Rejected ||
        current.status == OrderStatus::Expired)
      return Error::InvalidState;
    current.status = order.status;
    current.cumulative_quantity = order.cumulative_quantity;
    current.remaining_quantity = order.remaining_quantity;
    impl_->storage[existing].client_id.assign(order.client_order_id);
    impl_->storage[existing].venue_id.assign(order.venue_order_id);
    impl_->refresh_views(existing);
    return Error::Ok;
  }
  if (impl_->count == impl_->capacity) return Error::CapacityExceeded;
  const auto dense = static_cast<std::uint32_t>(impl_->count);
  impl_->views[dense] = order;
  impl_->storage[dense].client_id.assign(order.client_order_id);
  impl_->storage[dense].venue_id.assign(order.venue_order_id);
  impl_->refresh_views(dense);
  if (!impl_->add_index(order.token, dense)) {
    impl_->views[dense] = {};
    impl_->storage[dense] = {};
    return Error::CapacityExceeded;
  }
  ++impl_->count;
  return Error::Ok;
}

Error OrderManager::apply(const ExecutionUpdate& update) noexcept {
  const std::uint32_t dense = impl_->find_index(update.token);
  if (dense == kEmpty) return Error::NotFound;
  OrderView& order = impl_->views[dense];
  const auto refresh_ids = [&]() noexcept {
    if (update.client_order_id.length != 0) {
      impl_->storage[dense].client_id.assign(
          {update.client_order_id.value,
           std::min<std::size_t>(update.client_order_id.length,
                                 sizeof(update.client_order_id.value))});
    }
    if (update.venue_order_id.length != 0) {
      impl_->storage[dense].venue_id.assign(
          {update.venue_order_id.value,
           std::min<std::size_t>(update.venue_order_id.length,
                                 sizeof(update.venue_order_id.value))});
    }
    impl_->refresh_views(dense);
  };
  if (update.kind == ExecutionUpdate::Kind::Fill) {
    if (order.status == OrderStatus::Filled ||
        order.status == OrderStatus::Canceled ||
        order.status == OrderStatus::Rejected ||
        order.status == OrderStatus::Expired)
      return Error::InvalidState;
    if (update.fill_quantity.value <= 0 ||
        update.cumulative_quantity.scale != order.quantity.scale ||
        update.remaining_quantity.scale != order.quantity.scale ||
        update.cumulative_quantity.value <
            order.cumulative_quantity.value ||
        update.cumulative_quantity.value > order.quantity.value ||
        update.remaining_quantity.value < 0 ||
        update.remaining_quantity.value !=
            order.quantity.value - update.cumulative_quantity.value)
      return Error::InvalidState;
    refresh_ids();
    order.cumulative_quantity = update.cumulative_quantity;
    order.remaining_quantity = update.remaining_quantity;
    if (update.status == OrderStatus::Filled ||
        update.remaining_quantity.value == 0) {
      impl_->erase(dense);
      return Error::Ok;
    }
    order.status = OrderStatus::PartiallyFilled;
    return Error::Ok;
  }
  switch (update.status) {
    case OrderStatus::PendingSubmit:
      if (order.status != OrderStatus::PendingSubmit)
        return Error::InvalidState;
      refresh_ids();
      break;
    case OrderStatus::Open:
      if (order.status != OrderStatus::PendingSubmit &&
          order.status != OrderStatus::Open)
        return Error::InvalidState;
      refresh_ids();
      order.status = update.status;
      break;
    case OrderStatus::PartiallyFilled:
      if (order.status != OrderStatus::PendingSubmit &&
          order.status != OrderStatus::Open &&
          order.status != OrderStatus::PartiallyFilled)
        return Error::InvalidState;
      refresh_ids();
      order.status = update.status;
      break;
    case OrderStatus::Filled:
    case OrderStatus::Canceled:
    case OrderStatus::Rejected:
    case OrderStatus::Expired:
      impl_->erase(dense);
      break;
    case OrderStatus::Unknown:
      return Error::InvalidState;
  }
  return Error::Ok;
}

void OrderManager::clear() noexcept {
  std::fill(impl_->index.begin(), impl_->index.end(), kEmpty);
  std::fill(impl_->views.begin(), impl_->views.end(), OrderView{});
  std::fill(impl_->storage.begin(), impl_->storage.end(), Impl::Storage{});
  impl_->count = 0;
}

struct PositionManager::Impl {
  explicit Impl(std::size_t requested, std::size_t requested_dedup)
      : capacity(IsPowerOfTwo(requested) ? requested : 2),
        mask(capacity - 1U),
        views(capacity),
        index(capacity, kEmpty),
        dedup_capacity(IsPowerOfTwo(requested_dedup) ? requested_dedup : 2),
        dedup_mask(dedup_capacity - 1U),
        dedup_ids(dedup_capacity),
        dedup_accounts(dedup_capacity),
        dedup_index(dedup_capacity * 2U, kEmpty),
        dedup_index_mask(dedup_index.size() - 1U) {}

  [[nodiscard]] std::uint32_t find_index(
      AccountId account, InstrumentId instrument,
      PositionSide side) const noexcept {
    std::size_t slot =
        static_cast<std::size_t>(PositionHash(account, instrument, side)) &
        mask;
    for (std::size_t probe = 0; probe < capacity; ++probe) {
      const std::uint32_t value = index[slot];
      if (value == kEmpty) return kEmpty;
      if (value != kTombstone && value < count) {
        const PositionView& item = views[value];
        if (item.account_id == account && item.instrument_id == instrument &&
            item.side == side)
          return value;
      }
      slot = (slot + 1U) & mask;
    }
    return kEmpty;
  }

  [[nodiscard]] bool add_index(const PositionView& value,
                               std::uint32_t dense) noexcept {
    std::size_t slot = static_cast<std::size_t>(
                           PositionHash(value.account_id, value.instrument_id,
                                        value.side)) &
                       mask;
    for (std::size_t probe = 0; probe < capacity; ++probe) {
      if (index[slot] == kEmpty || index[slot] == kTombstone) {
        index[slot] = dense;
        return true;
      }
      slot = (slot + 1U) & mask;
    }
    return false;
  }

  void remove_index(const PositionView& value) noexcept {
    std::size_t slot = static_cast<std::size_t>(
                           PositionHash(value.account_id, value.instrument_id,
                                        value.side)) &
                       mask;
    for (std::size_t probe = 0; probe < capacity; ++probe) {
      const std::uint32_t dense = index[slot];
      if (dense == kEmpty) return;
      if (dense != kTombstone && dense < count) {
        const PositionView& current = views[dense];
        if (current.account_id == value.account_id &&
            current.instrument_id == value.instrument_id &&
            current.side == value.side) {
          index[slot] = kTombstone;
          return;
        }
      }
      slot = (slot + 1U) & mask;
    }
  }

  void update_dense_index(const PositionView& value,
                          std::uint32_t dense) noexcept {
    std::size_t slot = static_cast<std::size_t>(
                           PositionHash(value.account_id, value.instrument_id,
                                        value.side)) &
                       mask;
    for (std::size_t probe = 0; probe < capacity; ++probe) {
      const std::uint32_t current_dense = index[slot];
      if (current_dense == kEmpty) return;
      if (current_dense != kTombstone && current_dense < count) {
        const PositionView& current = views[current_dense];
        if (current.account_id == value.account_id &&
            current.instrument_id == value.instrument_id &&
            current.side == value.side) {
          index[slot] = dense;
          return;
        }
      }
      slot = (slot + 1U) & mask;
    }
  }

  void erase(std::uint32_t dense) noexcept {
    remove_index(views[dense]);
    const auto last = static_cast<std::uint32_t>(count - 1U);
    if (dense != last) {
      views[dense] = views[last];
      update_dense_index(views[dense], dense);
    }
    views[last] = {};
    --count;
  }

  [[nodiscard]] std::uint32_t find_fill(
      AccountId account, const TradeId& trade) const noexcept {
    std::size_t slot =
        static_cast<std::size_t>(TradeHash(account, trade)) &
        dedup_index_mask;
    for (std::size_t probe = 0; probe < dedup_index.size(); ++probe) {
      const std::uint32_t dense = dedup_index[slot];
      if (dense == kEmpty) return kEmpty;
      if (dense != kTombstone && dense < dedup_count &&
          dedup_accounts[dense] == account &&
          dedup_ids[dense] == trade)
        return dense;
      slot = (slot + 1U) & dedup_index_mask;
    }
    return kEmpty;
  }

  void remove_fill(AccountId account, const TradeId& trade) noexcept {
    std::size_t slot =
        static_cast<std::size_t>(TradeHash(account, trade)) &
        dedup_index_mask;
    for (std::size_t probe = 0; probe < dedup_index.size(); ++probe) {
      const std::uint32_t dense = dedup_index[slot];
      if (dense == kEmpty) return;
      if (dense != kTombstone && dense < dedup_count &&
          dedup_accounts[dense] == account &&
          dedup_ids[dense] == trade) {
        dedup_index[slot] = kTombstone;
        return;
      }
      slot = (slot + 1U) & dedup_index_mask;
    }
  }

  [[nodiscard]] bool add_fill(AccountId account, const TradeId& trade,
                              std::uint32_t dense) noexcept {
    std::size_t slot =
        static_cast<std::size_t>(TradeHash(account, trade)) &
        dedup_index_mask;
    std::size_t tombstone = dedup_index.size();
    for (std::size_t probe = 0; probe < dedup_index.size(); ++probe) {
      if (dedup_index[slot] == kEmpty) {
        dedup_index[tombstone == dedup_index.size() ? slot : tombstone] =
            dense;
        return true;
      }
      if (dedup_index[slot] == kTombstone &&
          tombstone == dedup_index.size())
        tombstone = slot;
      slot = (slot + 1U) & dedup_index_mask;
    }
    if (tombstone != dedup_index.size()) {
      dedup_index[tombstone] = dense;
      return true;
    }
    return false;
  }

  [[nodiscard]] Error remember_fill(AccountId account,
                                    const TradeId& trade) noexcept {
    if (trade.length == 0 || trade.length > sizeof(trade.value))
      return Error::InvalidArgument;
    if (find_fill(account, trade) != kEmpty)
      return Error::InvalidState;
    std::uint32_t dense{};
    if (dedup_count < dedup_capacity) {
      dense = static_cast<std::uint32_t>(dedup_count++);
    } else {
      dense = static_cast<std::uint32_t>(dedup_next);
      remove_fill(dedup_accounts[dense], dedup_ids[dense]);
      dedup_next = (dedup_next + 1U) & dedup_mask;
    }
    dedup_accounts[dense] = account;
    dedup_ids[dense] = trade;
    return add_fill(account, trade, dense) ? Error::Ok
                                          : Error::CapacityExceeded;
  }

  std::size_t capacity;
  std::size_t mask;
  std::size_t count{};
  std::vector<PositionView> views;
  std::vector<std::uint32_t> index;
  std::size_t dedup_capacity;
  std::size_t dedup_mask;
  std::size_t dedup_count{};
  std::size_t dedup_next{};
  std::vector<TradeId> dedup_ids;
  std::vector<AccountId> dedup_accounts;
  std::vector<std::uint32_t> dedup_index;
  std::size_t dedup_index_mask;
};

PositionManager::PositionManager(std::size_t capacity,
                                 std::size_t fill_dedup_capacity)
    : impl_(std::make_unique<Impl>(capacity, fill_dedup_capacity)) {}
PositionManager::~PositionManager() = default;
PositionManager::PositionManager(PositionManager&&) noexcept = default;
PositionManager& PositionManager::operator=(PositionManager&&) noexcept =
    default;

std::span<const PositionView> PositionManager::positions() const noexcept {
  return {impl_->views.data(), impl_->count};
}

Result<PositionView> PositionManager::find(
    AccountId account_id, InstrumentId instrument_id,
    PositionSide side) const noexcept {
  const std::uint32_t dense =
      impl_->find_index(account_id, instrument_id, side);
  if (dense == kEmpty) return {{}, Error::NotFound};
  return {impl_->views[dense], Error::Ok};
}

std::size_t PositionManager::size() const noexcept { return impl_->count; }
std::size_t PositionManager::capacity() const noexcept {
  return impl_->capacity;
}

Error PositionManager::apply_fill(
    AccountId account_id, InstrumentId instrument_id, Side side,
    PositionSide position_side, const FixedPoint& quantity,
    const TradeId& trade_id, std::uint64_t generation) noexcept {
  std::uint32_t dense =
      impl_->find_index(account_id, instrument_id, position_side);
  if (dense == kEmpty && impl_->count == impl_->capacity)
    return Error::CapacityExceeded;
  std::int64_t amount{};
  const std::uint8_t position_scale =
      dense == kEmpty ? quantity.scale
                      : impl_->views[dense].quantity.scale;
  if (!Rescale(quantity.value, quantity.scale, position_scale, amount))
    return Error::InvalidArgument;
  const bool add = position_side == PositionSide::Short
                       ? side == Side::Sell
                       : side == Side::Buy;
  std::int64_t next{};
  const std::int64_t current =
      dense == kEmpty ? 0 : impl_->views[dense].quantity.value;
  if (!Adjust(current, amount, add, next))
    return Error::InvalidArgument;
  const Error remembered = impl_->remember_fill(account_id, trade_id);
  if (remembered != Error::Ok) return remembered;
  if (next == 0) {
    if (dense != kEmpty) impl_->erase(dense);
    return Error::Ok;
  }
  if (dense == kEmpty) {
    dense = static_cast<std::uint32_t>(impl_->count);
    PositionView value;
    value.account_id = account_id;
    value.instrument_id = instrument_id;
    value.side = position_side;
    value.quantity.scale = quantity.scale;
    value.generation = generation;
    impl_->views[dense] = value;
    if (!impl_->add_index(value, dense)) return Error::CapacityExceeded;
    ++impl_->count;
  }
  PositionView& position = impl_->views[dense];
  position.quantity.value = next;
  position.generation = generation;
  return Error::Ok;
}

void PositionManager::retire_instrument(
    InstrumentId instrument_id) noexcept {
  std::uint32_t dense = 0;
  while (dense < impl_->count) {
    if (impl_->views[dense].instrument_id == instrument_id) {
      impl_->erase(dense);
    } else {
      ++dense;
    }
  }
}

Error PositionManager::replace_snapshot(
    std::span<const PositionView> positions) noexcept {
  std::fill(impl_->index.begin(), impl_->index.end(), kEmpty);
  std::fill(impl_->views.begin(), impl_->views.end(), PositionView{});
  impl_->count = 0;
  for (const PositionView& value : positions) {
    if (value.quantity.value == 0) continue;
    if (impl_->count == impl_->capacity) {
      std::fill(impl_->index.begin(), impl_->index.end(), kEmpty);
      std::fill(impl_->views.begin(), impl_->views.end(), PositionView{});
      impl_->count = 0;
      return Error::CapacityExceeded;
    }
    if (impl_->find_index(value.account_id, value.instrument_id, value.side) !=
        kEmpty) {
      std::fill(impl_->index.begin(), impl_->index.end(), kEmpty);
      std::fill(impl_->views.begin(), impl_->views.end(), PositionView{});
      impl_->count = 0;
      return Error::InvalidState;
    }
    const auto dense = static_cast<std::uint32_t>(impl_->count);
    impl_->views[dense] = value;
    if (!impl_->add_index(value, dense)) {
      std::fill(impl_->index.begin(), impl_->index.end(), kEmpty);
      std::fill(impl_->views.begin(), impl_->views.end(), PositionView{});
      impl_->count = 0;
      return Error::CapacityExceeded;
    }
    ++impl_->count;
  }
  return Error::Ok;
}

AccountManager::AccountManager(OrderManager& orders,
                               PositionManager& positions) noexcept
    : orders_(orders), positions_(positions) {}

}  // namespace strategyframe

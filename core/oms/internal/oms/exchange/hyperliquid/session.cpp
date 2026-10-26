#include "oms/exchange/hyperliquid/session.h"

#include "oms/exchange/hyperliquid/protocol.h"
#include "oms/exchange/polymarket/crypto.h"

#include <charconv>
#include <limits>

namespace oms::exchange::hyperliquid {
namespace {

bool add_fixed(api::FixedPoint& total, api::FixedPoint value) noexcept {
  while (total.scale < value.scale) {
    if (total.value > std::numeric_limits<std::int64_t>::max() / 10 ||
        total.value < std::numeric_limits<std::int64_t>::min() / 10)
      return false;
    total.value *= 10;
    ++total.scale;
  }
  while (value.scale < total.scale) {
    if (value.value > std::numeric_limits<std::int64_t>::max() / 10 ||
        value.value < std::numeric_limits<std::int64_t>::min() / 10)
      return false;
    value.value *= 10;
    ++value.scale;
  }
  if ((value.value > 0 &&
       total.value > std::numeric_limits<std::int64_t>::max() - value.value) ||
      (value.value < 0 &&
       total.value < std::numeric_limits<std::int64_t>::min() - value.value))
    return false;
  total.value += value.value;
  return true;
}

}  // namespace

struct Session::Signer {
  explicit Signer(std::string_view private_key)
      : context(private_key) {}
  polymarket::OrderSigningContext context;
};

Session::Session() : process_epoch_(make_process_epoch()) {}
Session::Session(std::uint32_t process_epoch)
    : process_epoch_(process_epoch & 0x00ffffffU) {}
Session::~Session() = default;

bool Session::arm_signer(std::string_view private_key) {
  signer_ = std::make_unique<Signer>(private_key);
  signer_ok_ = signer_->context.status() == polymarket::CryptoResult::Ok;
  if (!signer_ok_) status_ = AdapterStatus::Failed;
  refresh();
  return signer_ok_;
}

void Session::set_strategy_instance(
    std::string_view strategy_instance) noexcept {
  strategy_instance_hash_ = strategy_instance.empty()
                                ? 0
                                : stable_strategy_hash(strategy_instance);
}

bool Session::add_perp(std::string_view coin, std::int64_t coin_index,
                       std::int64_t dex_index, std::string_view dex_name,
                       std::uint8_t size_decimals) {
  return assets_.add_perp(coin, coin_index, dex_index, dex_name,
                          size_decimals);
}

void Session::add_spot(std::string_view pair, std::int64_t index) {
  assets_.add_spot(pair, index);
}

void Session::seal_metadata() noexcept {
  metadata_ = true;
  refresh();
}

void Session::note_private_open() noexcept {
  private_open_ = true;
  refresh();
}

void Session::note_private_lost() noexcept {
  private_open_ = false;
  reconciled_ = false;
  if (status_ != AdapterStatus::Failed) status_ = AdapterStatus::Reconciling;
}

bool Session::complete_reconcile(bool orders_mapped,
                                 bool positions_mapped) noexcept {
  reconciled_ = orders_mapped && positions_mapped;
  if (!reconciled_) status_ = AdapterStatus::Reconciling;
  refresh();
  return reconciled_;
}

void Session::refresh() noexcept {
  if (status_ == AdapterStatus::Failed) return;
  if (metadata_ && signer_ok_ && private_open_ && reconciled_) {
    status_ = AdapterStatus::Ready;
  } else if (status_ != AdapterStatus::Reconciling) {
    status_ = AdapterStatus::Connecting;
  }
}

std::uint64_t Session::next_nonce(std::uint64_t now_ms) noexcept {
  std::uint64_t observed = last_nonce_.load(std::memory_order_relaxed);
  for (;;) {
    if (observed == std::numeric_limits<std::uint64_t>::max()) return 0;
    const std::uint64_t candidate =
        now_ms > observed ? now_ms : observed + 1;
    if (last_nonce_.compare_exchange_weak(
            observed, candidate, std::memory_order_relaxed,
            std::memory_order_relaxed))
      return candidate;
  }
}

SignedAction Session::place_limit(std::uint64_t token, std::string_view coin,
                                  bool buy, std::string price, std::string size,
                                  bool reduce_only, std::string tif,
                                  std::uint64_t now_ms, bool mainnet,
                                  std::string_view client_order_id) {
  SignedAction result;
  if (!ready() || price.empty() || size.empty() || tif.empty()) return result;
  AssetId asset;
  if (!assets_.find(coin, asset)) return result;
  LimitOrder order;
  order.asset = asset.value;
  order.buy = buy;
  order.price = std::move(price);
  order.size = std::move(size);
  order.reduce_only = reduce_only;
  order.tif = std::move(tif);
  const std::uint32_t owner =
      strategy_instance_hash_ != 0
          ? strategy_instance_hash_
          : stable_strategy_hash(client_order_id.empty()
                                     ? std::string_view("self-quant")
                                     : client_order_id);
  if (!encode_cloid({owner, process_epoch_, token}, order.cloid))
    return result;
  result.action = encode_order_action(order);
  result.nonce = next_nonce(now_ms);
  if (result.nonce == 0) return {};
  result.cloid = order.cloid;
  result.sequence = token;
  result.asset = asset.value;
  const auto connection =
      action_hash(result.action.data(), result.action.size(), result.nonce,
                  nullptr);
  const auto digest = agent_digest(connection, mainnet);
  if (signer_->context.SignRecoverable(digest, result.signature) !=
      polymarket::CryptoResult::Ok) {
    return {};
  }
  Order stored;
  stored.asset = asset.value;
  stored.cloid = order.cloid;
  orders_[token] = stored;
  result.accepted = true;
  return result;
}

SignedAction Session::cancel_by_cloid(std::uint64_t token, std::uint64_t now_ms,
                                      bool mainnet) {
  SignedAction result;
  if (!signer_ok_ || signer_ == nullptr) return result;
  const auto found = orders_.find(token);
  if (found == orders_.end() || found->second.canceled) return result;
  result.action =
      encode_cancel_by_cloid(found->second.asset, found->second.cloid);
  result.nonce = next_nonce(now_ms);
  if (result.nonce == 0) return {};
  result.cloid = found->second.cloid;
  result.sequence = token;
  result.asset = found->second.asset;
  const auto connection =
      action_hash(result.action.data(), result.action.size(), result.nonce,
                  nullptr);
  const auto digest = agent_digest(connection, mainnet);
  if (signer_->context.SignRecoverable(digest, result.signature) !=
      polymarket::CryptoResult::Ok) {
    return {};
  }
  found->second.canceled = true;
  result.accepted = true;
  return result;
}

SignedAction Session::cancel_by_cloid(std::string_view coin,
                                      std::string_view cloid,
                                      std::uint64_t now_ms, bool mainnet) {
  SignedAction result;
  if (!signer_ok_ || signer_ == nullptr || cloid.empty()) return result;
  AssetId asset;
  if (!assets_.find(coin, asset)) return result;
  result.action = encode_cancel_by_cloid(asset.value, cloid);
  result.nonce = next_nonce(now_ms);
  if (result.nonce == 0) return {};
  result.cloid.assign(cloid);
  result.asset = asset.value;
  const auto connection =
      action_hash(result.action.data(), result.action.size(), result.nonce,
                  nullptr);
  const auto digest = agent_digest(connection, mainnet);
  if (signer_->context.SignRecoverable(digest, result.signature) !=
      polymarket::CryptoResult::Ok) {
    return {};
  }
  result.accepted = true;
  return result;
}

bool Session::cancel(std::uint64_t cloid) noexcept {
  const auto found = orders_.find(cloid);
  if (found == orders_.end() || found->second.canceled) return false;
  found->second.canceled = true;
  return true;
}

Session::FillResult Session::apply_fill(std::uint64_t trade_id,
                                        std::uint64_t cloid,
                                        std::int64_t quantity) noexcept {
  std::array<char, 24> text{};
  const auto converted =
      std::to_chars(text.data(), text.data() + text.size(), trade_id);
  if (converted.ec != std::errc{}) return FillResult::Unknown;
  return apply_fill(0, {},
                    std::string_view(
                        text.data(),
                        static_cast<std::size_t>(converted.ptr - text.data())),
                    cloid,
                    api::FixedPoint{quantity, 0, {}});
}

std::size_t Session::FillKeyHash::operator()(
    const FillKey& key) const noexcept {
  std::size_t hash = static_cast<std::size_t>(key.time_ms);
  const auto mix = [&hash](std::string_view text) {
    for (const char raw_value : text) {
      const auto value = static_cast<unsigned char>(raw_value);
      hash = (hash ^ value) * static_cast<std::size_t>(1099511628211ULL);
    }
  };
  mix(key.coin);
  mix(key.trade_id);
  return hash;
}

Session::FillResult Session::apply_fill(
    std::uint64_t time_ms, std::string_view coin, std::string_view trade_id,
    std::uint64_t cloid, api::FixedPoint quantity) noexcept {
  FillKey key;
  key.time_ms = time_ms;
  try {
    key.coin.assign(coin);
    key.trade_id.assign(trade_id);
    if (!fills_.insert(std::move(key)).second) return FillResult::Duplicate;
  } catch (...) {
    status_ = AdapterStatus::Failed;
    return FillResult::Unknown;
  }
  const auto found = orders_.find(cloid);
  if (found == orders_.end()) return FillResult::Unknown;
  if (!add_fixed(found->second.filled, quantity)) {
    status_ = AdapterStatus::Failed;
    return FillResult::Unknown;
  }
  return FillResult::Applied;
}

std::int64_t Session::filled(std::uint64_t cloid) const noexcept {
  const auto found = orders_.find(cloid);
  return found == orders_.end() ? 0 : found->second.filled.value;
}

bool Session::sequence_from_cloid(std::string_view cloid,
                                  std::uint64_t& sequence) const noexcept {
  CloidFields decoded{};
  if (!decode_cloid(cloid, decoded) ||
      (strategy_instance_hash_ != 0 &&
       decoded.strategy_instance_hash != strategy_instance_hash_))
    return false;
  sequence = decoded.sequence;
  return true;
}

void Session::note_unmapped() noexcept {
  reconciled_ = false;
  if (status_ != AdapterStatus::Failed) status_ = AdapterStatus::Reconciling;
}

}  // namespace oms::exchange::hyperliquid

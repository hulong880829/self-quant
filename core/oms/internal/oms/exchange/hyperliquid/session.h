#pragma once

#include <array>
#include <atomic>
#include <cstdint>
#include <memory>
#include <string>
#include <string_view>
#include <unordered_map>
#include <unordered_set>
#include <vector>

#include "oms/exchange/adapter_types.h"
#include "oms/exchange/hyperliquid/metadata.h"

namespace oms::exchange::hyperliquid {

struct SignedAction {
  bool accepted{};
  std::uint64_t nonce{};
  std::string cloid;
  std::uint64_t sequence{};
  std::int64_t asset{};
  std::vector<std::uint8_t> action{};
  std::array<std::uint8_t, 65> signature{};
};

// Logical Hyperliquid session. Network reads are injected; Ready requires
// metadata, a signer, and the private channel.
class Session {
 public:
  Session();
  explicit Session(std::uint32_t process_epoch);
  ~Session();
  Session(const Session&) = delete;
  Session& operator=(const Session&) = delete;

  [[nodiscard]] AdapterStatus status() const noexcept { return status_; }
  [[nodiscard]] bool ready() const noexcept {
    return status_ == AdapterStatus::Ready;
  }

  [[nodiscard]] bool arm_signer(std::string_view private_key);
  void set_strategy_instance(std::string_view strategy_instance) noexcept;
  [[nodiscard]] bool add_perp(
      std::string_view coin, std::int64_t coin_index,
      std::int64_t dex_index = 0, std::string_view dex_name = {},
      std::uint8_t size_decimals = 0);
  void add_spot(std::string_view pair, std::int64_t index);
  void seal_metadata() noexcept;
  void note_private_open() noexcept;
  void note_private_lost() noexcept;
  [[nodiscard]] bool complete_reconcile(bool orders_mapped,
                                        bool positions_mapped) noexcept;

  [[nodiscard]] SignedAction place_limit(std::uint64_t token,
                                         std::string_view coin, bool buy,
                                         std::string price, std::string size,
                                         bool reduce_only, std::string tif,
                                         std::uint64_t now_ms, bool mainnet,
                                         std::string_view client_order_id = {});
  [[nodiscard]] AdapterResult place_market() const noexcept {
    return AdapterResult::Unsupported;
  }
  [[nodiscard]] bool cancel(std::uint64_t cloid) noexcept;
  [[nodiscard]] SignedAction cancel_by_cloid(std::uint64_t token,
                                             std::uint64_t now_ms, bool mainnet);
  [[nodiscard]] SignedAction cancel_by_cloid(std::string_view coin,
                                             std::string_view cloid,
                                             std::uint64_t now_ms,
                                             bool mainnet);

  enum class FillResult : std::uint8_t { Applied, Duplicate, Unknown };
  [[nodiscard]] FillResult apply_fill(std::uint64_t time_ms,
                                      std::string_view coin,
                                      std::string_view trade_id,
                                      std::uint64_t cloid,
                                      api::FixedPoint quantity) noexcept;
  [[nodiscard]] FillResult apply_fill(std::uint64_t trade_id,
                                      std::uint64_t cloid,
                                      std::int64_t quantity) noexcept;
  [[nodiscard]] std::int64_t filled(std::uint64_t cloid) const noexcept;
  [[nodiscard]] bool sequence_from_cloid(std::string_view cloid,
                                         std::uint64_t& sequence) const noexcept;
  void note_unmapped() noexcept;

  [[nodiscard]] const AssetTable& assets() const noexcept { return assets_; }

 private:
  void refresh() noexcept;
  [[nodiscard]] std::uint64_t next_nonce(std::uint64_t now_ms) noexcept;

  struct Order {
    std::int64_t asset{};
    api::FixedPoint filled{};
    std::string cloid;
    bool canceled{};
  };

  struct FillKey {
    std::uint64_t time_ms{};
    std::string coin;
    std::string trade_id;

    friend bool operator==(const FillKey&, const FillKey&) = default;
  };

  struct FillKeyHash {
    [[nodiscard]] std::size_t operator()(const FillKey& key) const noexcept;
  };

  AssetTable assets_;
  bool metadata_{};
  bool signer_ok_{};
  bool private_open_{};
  bool reconciled_{};
  AdapterStatus status_{AdapterStatus::Connecting};
  std::uint32_t process_epoch_{};
  std::uint32_t strategy_instance_hash_{};
  std::atomic<std::uint64_t> last_nonce_{0};
  std::unordered_map<std::uint64_t, Order> orders_;
  std::unordered_set<FillKey, FillKeyHash> fills_;
  struct Signer;
  std::unique_ptr<Signer> signer_;
};

}  // namespace oms::exchange::hyperliquid

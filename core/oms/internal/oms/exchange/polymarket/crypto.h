#pragma once

#include <array>
#include <cstddef>
#include <cstdint>
#include <string_view>

namespace oms::exchange::polymarket {

using Bytes32 = std::array<std::uint8_t, 32>;
using Address = std::array<std::uint8_t, 20>;

enum class CryptoResult : std::uint8_t {
  Ok = 0,
  InvalidArgument = 1,
  BufferTooSmall = 2,
  UnsupportedSignatureType = 3,
  CryptoFailure = 4,
};

struct Order {
  std::uint64_t salt{};
  Address maker{};
  Address signer{};
  Bytes32 token_id{};
  std::uint64_t maker_amount{};
  std::uint64_t taker_amount{};
  std::uint8_t side{};
  std::uint8_t signature_type{};
  std::uint64_t timestamp_ms{};
  Bytes32 metadata{};
  Bytes32 builder{};
  bool negative_risk{};
};

struct Signature {
  // Type 0 uses 65 bytes. Type 3 appends the ERC-7739 domain, contents,
  // canonical type string, and two-byte big-endian type-string length.
  std::array<std::uint8_t, 512> bytes{};
  std::size_t size{};
};

// Prewarms all OpenSSL secp256k1 state needed for order signing. The context
// owns mutable scratch values and is intentionally single-owner and
// non-thread-safe.
class OrderSigningContext final {
 public:
  explicit OrderSigningContext(std::string_view private_key_hex) noexcept;
  ~OrderSigningContext() noexcept;

  OrderSigningContext(const OrderSigningContext&) = delete;
  OrderSigningContext& operator=(const OrderSigningContext&) = delete;
  OrderSigningContext(OrderSigningContext&&) = delete;
  OrderSigningContext& operator=(OrderSigningContext&&) = delete;

  [[nodiscard]] CryptoResult status() const noexcept;
  [[nodiscard]] CryptoResult Sign(const Order& order,
                                  Signature& output) noexcept;

 private:
  [[nodiscard]] CryptoResult SignDigest(
      const Bytes32& digest,
      std::array<std::uint8_t, 65>& signature) noexcept;

  struct Impl;
  Impl* impl_{};
  CryptoResult status_{CryptoResult::CryptoFailure};
};

[[nodiscard]] CryptoResult DecodeAddress(std::string_view text,
                                         Address& output) noexcept;
[[nodiscard]] CryptoResult DecodeHex32(std::string_view text,
                                       Bytes32& output) noexcept;
[[nodiscard]] CryptoResult ValidatePrivateKey(
    std::string_view private_key_hex) noexcept;
[[nodiscard]] CryptoResult TokenIdFromDecimal(std::string_view decimal,
                                              Bytes32& output) noexcept;
[[nodiscard]] CryptoResult TokenIdToDecimal(
    const Bytes32& token_id, char* output, std::size_t capacity,
    std::size_t& size) noexcept;

[[nodiscard]] CryptoResult L2Signature(
    std::string_view secret_base64url, std::string_view timestamp,
    std::string_view method, std::string_view path, std::string_view body,
    char* output, std::size_t capacity, std::size_t& size) noexcept;

[[nodiscard]] Bytes32 Keccak256(const std::uint8_t* data,
                                std::size_t size) noexcept;
[[nodiscard]] Bytes32 OrderDomainHash(bool negative_risk) noexcept;
[[nodiscard]] Bytes32 OrderStructHash(const Order& order) noexcept;
[[nodiscard]] Bytes32 OrderSigningHash(const Order& order) noexcept;

// Signature type 1 is intentionally an explicit unsupported legacy path.
// It is never encoded or signed as deposit-wallet type 3.
[[nodiscard]] CryptoResult SignOrder(
    const Order& order, std::string_view private_key_hex,
    Signature& output) noexcept;
[[nodiscard]] CryptoResult SignOrder(
    const Order& order, OrderSigningContext& context,
    Signature& output) noexcept;

[[nodiscard]] CryptoResult GenerateSafeSalt(std::uint64_t& salt) noexcept;

}  // namespace oms::exchange::polymarket

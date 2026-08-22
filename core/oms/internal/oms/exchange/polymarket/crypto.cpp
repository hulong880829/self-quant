#include "oms/exchange/polymarket/crypto.h"

#include <algorithm>
#include <array>
#include <cctype>
#include <cstring>
#include <limits>
#include <memory>
#include <new>

#include <openssl/bn.h>
#include <openssl/ec.h>
#include <openssl/evp.h>
#include <openssl/hmac.h>
#include <openssl/rand.h>

namespace oms::exchange::polymarket {
namespace {

constexpr std::string_view kDomainType =
    "EIP712Domain(string name,string version,uint256 chainId,address "
    "verifyingContract)";
constexpr std::string_view kOrderType =
    "Order(uint256 salt,address maker,address signer,uint256 tokenId,uint256 "
    "makerAmount,uint256 takerAmount,uint8 side,uint8 signatureType,uint256 "
    "timestamp,bytes32 metadata,bytes32 builder)";
constexpr std::string_view kDepositType =
    "TypedDataSign(Order contents,string name,string version,uint256 "
    "chainId,address verifyingContract,bytes32 salt)Order(uint256 salt,address "
    "maker,address signer,uint256 tokenId,uint256 makerAmount,uint256 "
    "takerAmount,uint8 side,uint8 signatureType,uint256 timestamp,bytes32 "
    "metadata,bytes32 builder)";
constexpr std::string_view kDomainName = "Polymarket CTF Exchange";
constexpr std::string_view kDomainVersion = "2";
constexpr std::string_view kDepositName = "DepositWallet";
constexpr std::string_view kDepositVersion = "1";
constexpr std::string_view kExchange =
    "0xE111180000d2663C0091e4f400237545B87B996B";
constexpr std::string_view kNegRiskExchange =
    "0xe2222d279d744050d28e00520010520000310F59";

using Bn = std::unique_ptr<BIGNUM, decltype(&BN_free)>;
using BnCtx = std::unique_ptr<BN_CTX, decltype(&BN_CTX_free)>;
using EcGroup = std::unique_ptr<EC_GROUP, decltype(&EC_GROUP_free)>;
using EcPoint = std::unique_ptr<EC_POINT, decltype(&EC_POINT_free)>;

constexpr std::uint64_t Rotl(std::uint64_t value, unsigned bits) noexcept {
  return bits == 0U ? value : (value << bits) | (value >> (64U - bits));
}

void KeccakPermutation(std::uint64_t state[25]) noexcept {
  static constexpr std::array<std::uint64_t, 24> kRound{
      0x0000000000000001ULL, 0x0000000000008082ULL,
      0x800000000000808aULL, 0x8000000080008000ULL,
      0x000000000000808bULL, 0x0000000080000001ULL,
      0x8000000080008081ULL, 0x8000000000008009ULL,
      0x000000000000008aULL, 0x0000000000000088ULL,
      0x0000000080008009ULL, 0x000000008000000aULL,
      0x000000008000808bULL, 0x800000000000008bULL,
      0x8000000000008089ULL, 0x8000000000008003ULL,
      0x8000000000008002ULL, 0x8000000000000080ULL,
      0x000000000000800aULL, 0x800000008000000aULL,
      0x8000000080008081ULL, 0x8000000000008080ULL,
      0x0000000080000001ULL, 0x8000000080008008ULL};
  static constexpr std::array<unsigned, 24> kRotation{
      1,  3,  6,  10, 15, 21, 28, 36, 45, 55, 2,  14,
      27, 41, 56, 8,  25, 43, 62, 18, 39, 61, 20, 44};
  static constexpr std::array<unsigned, 24> kLane{
      10, 7, 11, 17, 18, 3, 5, 16, 8, 21, 24, 4,
      15, 23, 19, 13, 12, 2, 20, 14, 22, 9, 6, 1};
  for (std::size_t round = 0; round < kRound.size(); ++round) {
    std::uint64_t column[5]{};
    for (std::size_t x = 0; x < 5; ++x) {
      column[x] = state[x] ^ state[x + 5] ^ state[x + 10] ^ state[x + 15] ^
                  state[x + 20];
    }
    for (std::size_t x = 0; x < 5; ++x) {
      const std::uint64_t delta =
          column[(x + 4) % 5] ^ Rotl(column[(x + 1) % 5], 1);
      for (std::size_t y = 0; y < 25; y += 5) state[y + x] ^= delta;
    }
    std::uint64_t current = state[1];
    for (std::size_t index = 0; index < kLane.size(); ++index) {
      const unsigned lane = kLane[index];
      const std::uint64_t next = state[lane];
      state[lane] = Rotl(current, kRotation[index]);
      current = next;
    }
    for (std::size_t y = 0; y < 25; y += 5) {
      std::uint64_t row[5]{};
      std::copy_n(state + y, 5, row);
      for (std::size_t x = 0; x < 5; ++x) {
        state[y + x] = row[x] ^ ((~row[(x + 1) % 5]) & row[(x + 2) % 5]);
      }
    }
    state[0] ^= kRound[round];
  }
}

int HexNibble(char value) noexcept {
  if (value >= '0' && value <= '9') return value - '0';
  if (value >= 'a' && value <= 'f') return value - 'a' + 10;
  if (value >= 'A' && value <= 'F') return value - 'A' + 10;
  return -1;
}

template <std::size_t Size>
CryptoResult DecodeHex(std::string_view text,
                       std::array<std::uint8_t, Size>& output) noexcept {
  if (text.starts_with("0x") || text.starts_with("0X")) text.remove_prefix(2);
  if (text.size() != Size * 2) return CryptoResult::InvalidArgument;
  for (std::size_t index = 0; index < Size; ++index) {
    const int high = HexNibble(text[index * 2]);
    const int low = HexNibble(text[index * 2 + 1]);
    if (high < 0 || low < 0) return CryptoResult::InvalidArgument;
    output[index] = static_cast<std::uint8_t>((high << 4) | low);
  }
  return CryptoResult::Ok;
}

void PutUint(Bytes32& word, std::uint64_t value) noexcept {
  for (std::size_t index = 0; index < 8; ++index) {
    word[31 - index] = static_cast<std::uint8_t>(value & 0xffU);
    value >>= 8U;
  }
}

void PutAddress(Bytes32& word, const Address& address) noexcept {
  std::copy(address.begin(), address.end(), word.begin() + 12);
}

template <std::size_t Capacity>
bool Append(std::array<std::uint8_t, Capacity>& output, std::size_t& used,
            const std::uint8_t* value, std::size_t size) noexcept {
  if (size > Capacity - used) return false;
  std::copy_n(value, size, output.begin() + static_cast<std::ptrdiff_t>(used));
  used += size;
  return true;
}

Bytes32 HashText(std::string_view value) noexcept {
  return Keccak256(reinterpret_cast<const std::uint8_t*>(value.data()),
                   value.size());
}

Bytes32 DomainHash(const Address& contract) noexcept {
  std::array<std::uint8_t, 160> encoded{};
  const Bytes32 type_hash = HashText(kDomainType);
  const Bytes32 name_hash = HashText(kDomainName);
  const Bytes32 version_hash = HashText(kDomainVersion);
  Bytes32 chain{};
  PutUint(chain, 137);
  Bytes32 address{};
  PutAddress(address, contract);
  std::copy(type_hash.begin(), type_hash.end(), encoded.begin());
  std::copy(name_hash.begin(), name_hash.end(), encoded.begin() + 32);
  std::copy(version_hash.begin(), version_hash.end(), encoded.begin() + 64);
  std::copy(chain.begin(), chain.end(), encoded.begin() + 96);
  std::copy(address.begin(), address.end(), encoded.begin() + 128);
  return Keccak256(encoded.data(), encoded.size());
}

Bytes32 TypedDigest(const Bytes32& domain, const Bytes32& content) noexcept {
  std::array<std::uint8_t, 66> encoded{};
  encoded[0] = 0x19;
  encoded[1] = 0x01;
  std::copy(domain.begin(), domain.end(), encoded.begin() + 2);
  std::copy(content.begin(), content.end(), encoded.begin() + 34);
  return Keccak256(encoded.data(), encoded.size());
}

bool HmacSha256(const std::uint8_t* key, std::size_t key_size,
                const std::uint8_t* data, std::size_t data_size,
                std::uint8_t output[32]) noexcept {
  if (key_size > static_cast<std::size_t>(std::numeric_limits<int>::max()))
    return false;
  unsigned length = 0;
  return HMAC(EVP_sha256(), key, static_cast<int>(key_size), data, data_size,
              output, &length) != nullptr &&
         length == 32;
}

bool DeterministicNonce(const Bytes32& private_key, const Bytes32& digest,
                        const BIGNUM* order, BIGNUM* nonce) noexcept {
  std::array<std::uint8_t, 32> key{};
  std::array<std::uint8_t, 32> value{};
  value.fill(1);
  std::array<std::uint8_t, 97> input{};
  std::copy(value.begin(), value.end(), input.begin());
  input[32] = 0;
  std::copy(private_key.begin(), private_key.end(), input.begin() + 33);
  std::copy(digest.begin(), digest.end(), input.begin() + 65);
  if (!HmacSha256(key.data(), key.size(), input.data(), input.size(),
                  key.data()) ||
      !HmacSha256(key.data(), key.size(), value.data(), value.size(),
                  value.data()))
    return false;
  std::copy(value.begin(), value.end(), input.begin());
  input[32] = 1;
  if (!HmacSha256(key.data(), key.size(), input.data(), input.size(),
                  key.data()) ||
      !HmacSha256(key.data(), key.size(), value.data(), value.size(),
                  value.data()))
    return false;
  for (;;) {
    if (!HmacSha256(key.data(), key.size(), value.data(), value.size(),
                    value.data()))
      return false;
    if (BN_bin2bn(value.data(), static_cast<int>(value.size()), nonce) ==
            nullptr ||
        (!BN_is_zero(nonce) && BN_cmp(nonce, order) < 0))
      return !BN_is_zero(nonce) && BN_cmp(nonce, order) < 0;
    std::array<std::uint8_t, 33> retry{};
    std::copy(value.begin(), value.end(), retry.begin());
    if (!HmacSha256(key.data(), key.size(), retry.data(), retry.size(),
                    key.data()) ||
        !HmacSha256(key.data(), key.size(), value.data(), value.size(),
                    value.data()))
      return false;
  }
}

int DecodeBase64Char(char value) noexcept {
  if (value >= 'A' && value <= 'Z') return value - 'A';
  if (value >= 'a' && value <= 'z') return value - 'a' + 26;
  if (value >= '0' && value <= '9') return value - '0' + 52;
  if (value == '-' || value == '+') return 62;
  if (value == '_' || value == '/') return 63;
  return -1;
}

CryptoResult DecodeBase64Url(std::string_view input, std::uint8_t* output,
                             std::size_t capacity,
                             std::size_t& size) noexcept {
  size = 0;
  if (input.empty()) return CryptoResult::InvalidArgument;
  const std::size_t first_padding = input.find('=');
  const std::size_t meaningful =
      first_padding == std::string_view::npos ? input.size() : first_padding;
  const std::size_t padding = input.size() - meaningful;
  if (meaningful % 4 == 1 || padding > 2 ||
      (padding != 0 && input.size() % 4 != 0) ||
      (padding == 1 && meaningful % 4 != 3) ||
      (padding == 2 && meaningful % 4 != 2))
    return CryptoResult::InvalidArgument;
  std::uint32_t accumulator = 0;
  unsigned bits = 0;
  bool saw_padding = false;
  for (const char value : input) {
    if (value == '=') {
      saw_padding = true;
      continue;
    }
    if (saw_padding) return CryptoResult::InvalidArgument;
    const int decoded = DecodeBase64Char(value);
    if (decoded < 0) return CryptoResult::InvalidArgument;
    accumulator = (accumulator << 6U) | static_cast<unsigned>(decoded);
    bits += 6;
    if (bits >= 8) {
      bits -= 8;
      if (size == capacity) return CryptoResult::BufferTooSmall;
      output[size++] =
          static_cast<std::uint8_t>((accumulator >> bits) & 0xffU);
    }
  }
  if (bits != 0 && (accumulator & ((1U << bits) - 1U)) != 0)
    return CryptoResult::InvalidArgument;
  return CryptoResult::Ok;
}

constexpr char kBase64Url[] =
    "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";

}  // namespace

struct OrderSigningContext::Impl {
  Impl() noexcept
      : group(EC_GROUP_new_by_curve_name(NID_secp256k1), EC_GROUP_free),
        context(BN_CTX_new(), BN_CTX_free),
        private_key(BN_new(), BN_free),
        order(BN_new(), BN_free),
        half_order(BN_new(), BN_free),
        nonce(BN_new(), BN_free),
        x(BN_new(), BN_free),
        y(BN_new(), BN_free),
        r(BN_new(), BN_free),
        s(BN_new(), BN_free),
        z(BN_new(), BN_free),
        inverse(BN_new(), BN_free),
        point(group ? EC_POINT_new(group.get()) : nullptr, EC_POINT_free) {}

  Bytes32 private_bytes{};
  EcGroup group;
  BnCtx context;
  Bn private_key;
  Bn order;
  Bn half_order;
  Bn nonce;
  Bn x;
  Bn y;
  Bn r;
  Bn s;
  Bn z;
  Bn inverse;
  EcPoint point;
};

OrderSigningContext::OrderSigningContext(
    std::string_view private_key_hex) noexcept {
  Bytes32 private_bytes{};
  if (DecodeHex(private_key_hex, private_bytes) != CryptoResult::Ok) {
    status_ = CryptoResult::InvalidArgument;
    return;
  }
  impl_ = new (std::nothrow) Impl;
  if (impl_ == nullptr || !impl_->group || !impl_->context ||
      !impl_->private_key || !impl_->order || !impl_->half_order ||
      !impl_->nonce || !impl_->x || !impl_->y || !impl_->r || !impl_->s ||
      !impl_->z || !impl_->inverse || !impl_->point ||
      BN_bin2bn(private_bytes.data(), static_cast<int>(private_bytes.size()),
                impl_->private_key.get()) == nullptr ||
      EC_GROUP_get_order(impl_->group.get(), impl_->order.get(),
                         impl_->context.get()) != 1 ||
      BN_rshift1(impl_->half_order.get(), impl_->order.get()) != 1) {
    status_ = CryptoResult::CryptoFailure;
    return;
  }
  if (BN_is_zero(impl_->private_key.get()) ||
      BN_cmp(impl_->private_key.get(), impl_->order.get()) >= 0) {
    status_ = CryptoResult::InvalidArgument;
    return;
  }
  impl_->private_bytes = private_bytes;
  status_ = CryptoResult::Ok;
}

OrderSigningContext::~OrderSigningContext() noexcept { delete impl_; }

CryptoResult OrderSigningContext::status() const noexcept { return status_; }

CryptoResult OrderSigningContext::SignDigest(
    const Bytes32& digest,
    std::array<std::uint8_t, 65>& signature) noexcept {
  if (status_ != CryptoResult::Ok || impl_ == nullptr) return status_;
  if (BN_bin2bn(digest.data(), static_cast<int>(digest.size()),
                impl_->z.get()) == nullptr ||
      !DeterministicNonce(impl_->private_bytes, digest, impl_->order.get(),
                          impl_->nonce.get()) ||
      EC_POINT_mul(impl_->group.get(), impl_->point.get(), impl_->nonce.get(),
                   nullptr, nullptr, impl_->context.get()) != 1 ||
      EC_POINT_get_affine_coordinates(
          impl_->group.get(), impl_->point.get(), impl_->x.get(), impl_->y.get(),
          impl_->context.get()) != 1 ||
      BN_nnmod(impl_->r.get(), impl_->x.get(), impl_->order.get(),
               impl_->context.get()) != 1 ||
      BN_is_zero(impl_->r.get()) ||
      BN_mod_inverse(impl_->inverse.get(), impl_->nonce.get(),
                     impl_->order.get(), impl_->context.get()) == nullptr ||
      BN_mod_mul(impl_->s.get(), impl_->r.get(), impl_->private_key.get(),
                 impl_->order.get(), impl_->context.get()) != 1 ||
      BN_mod_add(impl_->s.get(), impl_->s.get(), impl_->z.get(),
                 impl_->order.get(), impl_->context.get()) != 1 ||
      BN_mod_mul(impl_->s.get(), impl_->s.get(), impl_->inverse.get(),
                 impl_->order.get(), impl_->context.get()) != 1 ||
      BN_is_zero(impl_->s.get()))
    return CryptoResult::CryptoFailure;
  unsigned recovery = BN_is_odd(impl_->y.get()) ? 1U : 0U;
  if (BN_cmp(impl_->x.get(), impl_->order.get()) >= 0) recovery |= 2U;
  if (BN_cmp(impl_->s.get(), impl_->half_order.get()) > 0) {
    if (BN_sub(impl_->s.get(), impl_->order.get(), impl_->s.get()) != 1)
      return CryptoResult::CryptoFailure;
    recovery ^= 1U;
  }
  if (BN_bn2binpad(impl_->r.get(), signature.data(), 32) != 32 ||
      BN_bn2binpad(impl_->s.get(), signature.data() + 32, 32) != 32)
    return CryptoResult::CryptoFailure;
  signature[64] = static_cast<std::uint8_t>(27U + recovery);
  return CryptoResult::Ok;
}

Bytes32 Keccak256(const std::uint8_t* data, std::size_t size) noexcept {
  constexpr std::size_t rate = 136;
  std::uint64_t state[25]{};
  while (size >= rate) {
    for (std::size_t index = 0; index < rate; ++index) {
      state[index / 8] ^=
          static_cast<std::uint64_t>(data[index]) << ((index % 8) * 8);
    }
    KeccakPermutation(state);
    data += rate;
    size -= rate;
  }
  std::array<std::uint8_t, rate> block{};
  if (size != 0) std::copy_n(data, size, block.begin());
  block[size] = 0x01;
  block[rate - 1] |= 0x80;
  for (std::size_t index = 0; index < rate; ++index) {
    state[index / 8] ^=
        static_cast<std::uint64_t>(block[index]) << ((index % 8) * 8);
  }
  KeccakPermutation(state);
  Bytes32 output{};
  for (std::size_t index = 0; index < output.size(); ++index) {
    output[index] =
        static_cast<std::uint8_t>((state[index / 8] >> ((index % 8) * 8)) &
                                  0xffU);
  }
  return output;
}

CryptoResult DecodeAddress(std::string_view text, Address& output) noexcept {
  return DecodeHex(text, output);
}

CryptoResult DecodeHex32(std::string_view text, Bytes32& output) noexcept {
  return DecodeHex(text, output);
}

CryptoResult ValidatePrivateKey(std::string_view private_key_hex) noexcept {
  const OrderSigningContext context(private_key_hex);
  return context.status();
}

CryptoResult TokenIdFromDecimal(std::string_view decimal,
                                Bytes32& output) noexcept {
  output.fill(0);
  if (decimal.empty() || (decimal.size() > 1 && decimal.front() == '0'))
    return CryptoResult::InvalidArgument;
  for (const char value : decimal) {
    if (value < '0' || value > '9') return CryptoResult::InvalidArgument;
    unsigned carry = static_cast<unsigned>(value - '0');
    for (std::size_t index = output.size(); index-- > 0;) {
      const unsigned next = static_cast<unsigned>(output[index]) * 10U + carry;
      output[index] = static_cast<std::uint8_t>(next & 0xffU);
      carry = next >> 8U;
    }
    if (carry != 0) return CryptoResult::InvalidArgument;
  }
  return CryptoResult::Ok;
}

CryptoResult TokenIdToDecimal(const Bytes32& token_id, char* output,
                              std::size_t capacity,
                              std::size_t& size) noexcept {
  size = 0;
  if (output == nullptr || capacity == 0) return CryptoResult::BufferTooSmall;
  std::array<std::uint8_t, 32> value = token_id;
  std::array<char, 78> reversed{};
  do {
    unsigned remainder = 0;
    for (std::uint8_t& byte : value) {
      const unsigned current = (remainder << 8U) | byte;
      byte = static_cast<std::uint8_t>(current / 10U);
      remainder = current % 10U;
    }
    reversed[size++] = static_cast<char>('0' + remainder);
  } while (std::any_of(value.begin(), value.end(),
                       [](std::uint8_t byte) { return byte != 0; }));
  if (size + 1 > capacity) {
    size = 0;
    return CryptoResult::BufferTooSmall;
  }
  for (std::size_t index = 0; index < size; ++index)
    output[index] = reversed[size - index - 1];
  output[size] = '\0';
  return CryptoResult::Ok;
}

CryptoResult L2Signature(std::string_view secret_base64url,
                         std::string_view timestamp, std::string_view method,
                         std::string_view path, std::string_view body,
                         char* output, std::size_t capacity,
                         std::size_t& size) noexcept {
  size = 0;
  if (output == nullptr || timestamp.empty() || method.empty() || path.empty() ||
      path.front() != '/')
    return CryptoResult::InvalidArgument;
  if (capacity < 45) return CryptoResult::BufferTooSmall;
  std::array<std::uint8_t, 256> key{};
  std::size_t key_size = 0;
  CryptoResult result =
      DecodeBase64Url(secret_base64url, key.data(), key.size(), key_size);
  if (result != CryptoResult::Ok) return result;
  std::array<std::uint8_t, 8192> canonical{};
  std::size_t canonical_size = 0;
  const auto append_text = [&](std::string_view text, bool uppercase) {
    if (text.size() > canonical.size() - canonical_size) return false;
    for (const char value : text) {
      canonical[canonical_size++] = static_cast<std::uint8_t>(
          uppercase ? std::toupper(static_cast<unsigned char>(value)) : value);
    }
    return true;
  };
  if (!append_text(timestamp, false) || !append_text(method, true) ||
      !append_text(path, false) || !append_text(body, false))
    return CryptoResult::BufferTooSmall;
  std::uint8_t digest[32]{};
  if (!HmacSha256(key.data(), key_size, canonical.data(), canonical_size,
                  digest))
    return CryptoResult::CryptoFailure;
  for (std::size_t index = 0; index < 30; index += 3) {
    const std::uint32_t triple =
        (static_cast<std::uint32_t>(digest[index]) << 16U) |
        (static_cast<std::uint32_t>(digest[index + 1]) << 8U) |
        digest[index + 2];
    output[size++] = kBase64Url[(triple >> 18U) & 63U];
    output[size++] = kBase64Url[(triple >> 12U) & 63U];
    output[size++] = kBase64Url[(triple >> 6U) & 63U];
    output[size++] = kBase64Url[triple & 63U];
  }
  const std::uint32_t last =
      (static_cast<std::uint32_t>(digest[30]) << 16U) |
      (static_cast<std::uint32_t>(digest[31]) << 8U);
  output[size++] = kBase64Url[(last >> 18U) & 63U];
  output[size++] = kBase64Url[(last >> 12U) & 63U];
  output[size++] = kBase64Url[(last >> 6U) & 63U];
  output[size++] = '=';
  output[size] = '\0';
  return CryptoResult::Ok;
}

Bytes32 OrderDomainHash(bool negative_risk) noexcept {
  Address contract{};
  if (DecodeAddress(negative_risk ? kNegRiskExchange : kExchange, contract) !=
      CryptoResult::Ok)
    return {};
  return DomainHash(contract);
}

Bytes32 OrderStructHash(const Order& order) noexcept {
  std::array<std::uint8_t, 384> encoded{};
  const Bytes32 type_hash = HashText(kOrderType);
  std::copy(type_hash.begin(), type_hash.end(), encoded.begin());
  std::array<Bytes32, 11> fields{};
  PutUint(fields[0], order.salt);
  PutAddress(fields[1], order.maker);
  PutAddress(fields[2], order.signer);
  fields[3] = order.token_id;
  PutUint(fields[4], order.maker_amount);
  PutUint(fields[5], order.taker_amount);
  PutUint(fields[6], order.side);
  PutUint(fields[7], order.signature_type);
  PutUint(fields[8], order.timestamp_ms);
  fields[9] = order.metadata;
  fields[10] = order.builder;
  for (std::size_t index = 0; index < fields.size(); ++index) {
    std::copy(fields[index].begin(), fields[index].end(),
              encoded.begin() + static_cast<std::ptrdiff_t>((index + 1) * 32));
  }
  return Keccak256(encoded.data(), encoded.size());
}

Bytes32 OrderSigningHash(const Order& order) noexcept {
  if (order.signature_type != 0 && order.signature_type != 3) return {};
  const Bytes32 domain = OrderDomainHash(order.negative_risk);
  const Bytes32 contents = OrderStructHash(order);
  if (order.signature_type != 3) return TypedDigest(domain, contents);
  std::array<std::uint8_t, 224> encoded{};
  const Bytes32 type_hash = HashText(kDepositType);
  const Bytes32 name_hash = HashText(kDepositName);
  const Bytes32 version_hash = HashText(kDepositVersion);
  Bytes32 chain{};
  PutUint(chain, 137);
  Bytes32 contract{};
  PutAddress(contract, order.maker);
  std::copy(type_hash.begin(), type_hash.end(), encoded.begin());
  std::copy(contents.begin(), contents.end(), encoded.begin() + 32);
  std::copy(name_hash.begin(), name_hash.end(), encoded.begin() + 64);
  std::copy(version_hash.begin(), version_hash.end(), encoded.begin() + 96);
  std::copy(chain.begin(), chain.end(), encoded.begin() + 128);
  std::copy(contract.begin(), contract.end(), encoded.begin() + 160);
  const Bytes32 outer = Keccak256(encoded.data(), encoded.size());
  return TypedDigest(domain, outer);
}

CryptoResult OrderSigningContext::Sign(const Order& order,
                                       Signature& output) noexcept {
  output = {};
  if (order.side > 1 || (order.signature_type != 0 &&
                         order.signature_type != 1 &&
                         order.signature_type != 3))
    return CryptoResult::InvalidArgument;
  if (order.signature_type == 1)
    return CryptoResult::UnsupportedSignatureType;
  std::array<std::uint8_t, 65> signature{};
  const Bytes32 digest = OrderSigningHash(order);
  CryptoResult result = SignDigest(digest, signature);
  if (result != CryptoResult::Ok) return result;
  std::size_t used = 0;
  if (!Append(output.bytes, used, signature.data(), signature.size()))
    return CryptoResult::BufferTooSmall;
  if (order.signature_type == 3) {
    const Bytes32 domain = OrderDomainHash(order.negative_risk);
    const Bytes32 contents = OrderStructHash(order);
    if (!Append(output.bytes, used, domain.data(), domain.size()) ||
        !Append(output.bytes, used, contents.data(), contents.size()) ||
        !Append(output.bytes, used,
                reinterpret_cast<const std::uint8_t*>(kOrderType.data()),
                kOrderType.size()))
      return CryptoResult::BufferTooSmall;
    const std::array<std::uint8_t, 2> length{
        static_cast<std::uint8_t>((kOrderType.size() >> 8U) & 0xffU),
        static_cast<std::uint8_t>(kOrderType.size() & 0xffU)};
    if (!Append(output.bytes, used, length.data(), length.size()))
      return CryptoResult::BufferTooSmall;
  }
  output.size = used;
  return CryptoResult::Ok;
}

CryptoResult SignOrder(const Order& order, std::string_view private_key_hex,
                       Signature& output) noexcept {
  OrderSigningContext context(private_key_hex);
  return context.Sign(order, output);
}

CryptoResult SignOrder(const Order& order, OrderSigningContext& context,
                       Signature& output) noexcept {
  return context.Sign(order, output);
}

CryptoResult GenerateSafeSalt(std::uint64_t& salt) noexcept {
  if (RAND_bytes(reinterpret_cast<unsigned char*>(&salt),
                 static_cast<int>(sizeof(salt))) != 1)
    return CryptoResult::CryptoFailure;
  salt &= (std::uint64_t{1} << 53U) - 1U;
  if (salt == 0) salt = 1;
  return CryptoResult::Ok;
}

}  // namespace oms::exchange::polymarket

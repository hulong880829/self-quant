#include "oms/exchange/binance/protocol.h"

#include <algorithm>
#include <array>
#include <charconv>
#include <cstring>
#include <limits>

namespace oms::exchange::binance {
namespace {

constexpr std::uint64_t Bits(std::initializer_list<AdapterCapability> values) {
  std::uint64_t bits = 0;
  for (const auto value : values) bits |= static_cast<std::uint64_t>(value);
  return bits;
}

constexpr Profile kSpotProfile{
    Product::Spot,
    "https://api.binance.com",
    "wss://stream.binance.com:9443",
    "/api/v3/order",
    "/api/v3/order",
    "/api/v3/openOrders",
    "/api/v3/userDataStream",
    "/api/v3/time",
    {Bits({AdapterCapability::Limit, AdapterCapability::Market,
           AdapterCapability::TifGtc, AdapterCapability::TifIoc,
           AdapterCapability::TifFok, AdapterCapability::PostOnly,
           AdapterCapability::QuoteQuantity,
           AdapterCapability::ReconcileOpenOrders,
           AdapterCapability::UserOrderStream,
           AdapterCapability::UserFillStream})}};

constexpr Profile kUsdmProfile{
    Product::Usdm,
    "https://fapi.binance.com",
    "wss://fstream.binance.com",
    "/fapi/v1/order",
    "/fapi/v1/order",
    "/fapi/v1/openOrders",
    "/fapi/v1/listenKey",
    "/fapi/v1/time",
    {Bits({AdapterCapability::Limit, AdapterCapability::Market,
           AdapterCapability::TifGtc, AdapterCapability::TifGtd,
           AdapterCapability::TifIoc, AdapterCapability::TifFok,
           AdapterCapability::PostOnly, AdapterCapability::ReduceOnly,
           AdapterCapability::ReconcileOpenOrders,
           AdapterCapability::UserOrderStream,
           AdapterCapability::UserFillStream})}};

class Appender {
 public:
  explicit Appender(HttpRequest& request) : request_(request) {}

  bool append(std::string_view value) noexcept {
    if (value.size() > request_.target.size() - size_) return false;
    std::memcpy(request_.target.data() + size_, value.data(), value.size());
    size_ += value.size();
    return true;
  }

  template <typename Integer>
  bool integer(Integer value) noexcept {
    std::array<char, 32> buffer{};
    const auto result =
        std::to_chars(buffer.data(), buffer.data() + buffer.size(), value);
    return result.ec == std::errc{} &&
           append({buffer.data(), static_cast<std::size_t>(result.ptr -
                                                           buffer.data())});
  }

  bool encoded(std::string_view value) noexcept {
    constexpr char kHex[] = "0123456789ABCDEF";
    for (const char raw_character : value) {
      const auto character = static_cast<unsigned char>(raw_character);
      const bool unreserved =
          (character >= 'a' && character <= 'z') ||
          (character >= 'A' && character <= 'Z') ||
          (character >= '0' && character <= '9') || character == '-' ||
          character == '_' || character == '.' || character == '~';
      if (unreserved) {
        const char raw = static_cast<char>(character);
        if (!append({&raw, 1})) return false;
      } else {
        const std::array<char, 3> escape{
            '%', kHex[(character >> 4U) & 0x0fU], kHex[character & 0x0fU]};
        if (!append({escape.data(), escape.size()})) return false;
      }
    }
    return true;
  }

  bool fixed(api::FixedPoint value) noexcept {
    if (value.scale > 18) return false;
    std::array<char, 48> digits{};
    std::uint64_t magnitude =
        value.value < 0
            ? static_cast<std::uint64_t>(-(value.value + 1)) + 1U
            : static_cast<std::uint64_t>(value.value);
    const auto result =
        std::to_chars(digits.data(), digits.data() + digits.size(), magnitude);
    if (result.ec != std::errc{}) return false;
    const std::size_t count =
        static_cast<std::size_t>(result.ptr - digits.data());
    if (value.value < 0 && !append("-")) return false;
    if (value.scale == 0) return append({digits.data(), count});
    const std::size_t scale = value.scale;
    if (count <= scale) {
      if (!append("0.")) return false;
      for (std::size_t index = count; index < scale; ++index)
        if (!append("0")) return false;
      return append({digits.data(), count});
    }
    return append({digits.data(), count - scale}) && append(".") &&
           append({digits.data() + count - scale, scale});
  }

  [[nodiscard]] std::size_t size() const noexcept { return size_; }
  void finish() noexcept {
    request_.target_length = static_cast<std::uint16_t>(size_);
  }

 private:
  HttpRequest& request_;
  std::size_t size_{};
};

bool valid_token(std::string_view value) noexcept {
  if (value.empty()) return false;
  for (const char raw_character : value) {
    const auto character = static_cast<unsigned char>(raw_character);
    if (!((character >= 'a' && character <= 'z') ||
          (character >= 'A' && character <= 'Z') ||
          (character >= '0' && character <= '9') || character == '-' ||
          character == '_' || character == '.'))
      return false;
  }
  return true;
}

void initialize_request(HttpMethod method, CredentialsView credentials,
                        bool is_signed, HttpRequest& request) noexcept {
  request = {};
  request.method = method;
  constexpr std::string_view kHeader = "X-MBX-APIKEY";
  std::memcpy(request.api_key_header_name.data(), kHeader.data(),
              kHeader.size());
  request.api_key_header_name_length =
      static_cast<std::uint8_t>(kHeader.size());
  request.api_key_value = credentials.api_key;
  request.signed_request = is_signed;
}

bool append_signed_tail(Appender& appender, std::uint64_t timestamp_ms,
                        std::uint32_t recv_window_ms,
                        CredentialsView credentials,
                        HttpRequest& output) noexcept {
  if (credentials.api_key.empty() || credentials.secret_key.empty() ||
      recv_window_ms == 0 || recv_window_ms > 60000)
    return false;
  if (!appender.append("&timestamp=") || !appender.integer(timestamp_ms) ||
      !appender.append("&recvWindow=") || !appender.integer(recv_window_ms))
    return false;
  std::array<char, 64> signature{};
  const std::string_view target{output.target.data(), appender.size()};
  const std::size_t query = target.find('?');
  if (query == std::string_view::npos ||
      !hmac_sha256_hex(credentials.secret_key, target.substr(query + 1),
                       signature) ||
      !appender.append("&signature=") ||
      !appender.append({signature.data(), signature.size()}))
    return false;
  appender.finish();
  return true;
}

std::string_view side_name(api::Side side) noexcept {
  return side == api::Side::Buy ? "BUY" : "SELL";
}

std::string_view tif_name(api::TimeInForce tif) noexcept {
  switch (tif) {
    case api::TimeInForce::GTC:
      return "GTC";
    case api::TimeInForce::GTD:
      return "GTD";
    case api::TimeInForce::IOC:
      return "IOC";
    case api::TimeInForce::FOK:
      return "FOK";
    case api::TimeInForce::FAK:
      return {};
  }
  return {};
}

// Compact SHA-256 implementation used to keep protocol tests independent of
// the networking target and its OpenSSL linkage.
constexpr std::array<std::uint32_t, 64> kShaK{
    0x428a2f98U, 0x71374491U, 0xb5c0fbcfU, 0xe9b5dba5U, 0x3956c25bU,
    0x59f111f1U, 0x923f82a4U, 0xab1c5ed5U, 0xd807aa98U, 0x12835b01U,
    0x243185beU, 0x550c7dc3U, 0x72be5d74U, 0x80deb1feU, 0x9bdc06a7U,
    0xc19bf174U, 0xe49b69c1U, 0xefbe4786U, 0x0fc19dc6U, 0x240ca1ccU,
    0x2de92c6fU, 0x4a7484aaU, 0x5cb0a9dcU, 0x76f988daU, 0x983e5152U,
    0xa831c66dU, 0xb00327c8U, 0xbf597fc7U, 0xc6e00bf3U, 0xd5a79147U,
    0x06ca6351U, 0x14292967U, 0x27b70a85U, 0x2e1b2138U, 0x4d2c6dfcU,
    0x53380d13U, 0x650a7354U, 0x766a0abbU, 0x81c2c92eU, 0x92722c85U,
    0xa2bfe8a1U, 0xa81a664bU, 0xc24b8b70U, 0xc76c51a3U, 0xd192e819U,
    0xd6990624U, 0xf40e3585U, 0x106aa070U, 0x19a4c116U, 0x1e376c08U,
    0x2748774cU, 0x34b0bcb5U, 0x391c0cb3U, 0x4ed8aa4aU, 0x5b9cca4fU,
    0x682e6ff3U, 0x748f82eeU, 0x78a5636fU, 0x84c87814U, 0x8cc70208U,
    0x90befffaU, 0xa4506cebU, 0xbef9a3f7U, 0xc67178f2U};

constexpr std::uint32_t rotr(std::uint32_t value,
                             std::uint32_t count) noexcept {
  return (value >> count) | (value << (32U - count));
}

struct Sha256 {
  std::array<std::uint32_t, 8> state{
      0x6a09e667U, 0xbb67ae85U, 0x3c6ef372U, 0xa54ff53aU,
      0x510e527fU, 0x9b05688cU, 0x1f83d9abU, 0x5be0cd19U};
  std::array<std::uint8_t, 64> block{};
  std::size_t used{};
  std::uint64_t bytes{};

  void transform() noexcept {
    std::array<std::uint32_t, 64> words{};
    for (std::size_t index = 0; index < 16; ++index) {
      const std::size_t offset = index * 4;
      words[index] = (static_cast<std::uint32_t>(block[offset]) << 24U) |
                     (static_cast<std::uint32_t>(block[offset + 1]) << 16U) |
                     (static_cast<std::uint32_t>(block[offset + 2]) << 8U) |
                     static_cast<std::uint32_t>(block[offset + 3]);
    }
    for (std::size_t index = 16; index < words.size(); ++index) {
      const std::uint32_t s0 = rotr(words[index - 15], 7) ^
                               rotr(words[index - 15], 18) ^
                               (words[index - 15] >> 3U);
      const std::uint32_t s1 = rotr(words[index - 2], 17) ^
                               rotr(words[index - 2], 19) ^
                               (words[index - 2] >> 10U);
      words[index] = words[index - 16] + s0 + words[index - 7] + s1;
    }
    auto work = state;
    for (std::size_t index = 0; index < words.size(); ++index) {
      const std::uint32_t s1 =
          rotr(work[4], 6) ^ rotr(work[4], 11) ^ rotr(work[4], 25);
      const std::uint32_t choice =
          (work[4] & work[5]) ^ ((~work[4]) & work[6]);
      const std::uint32_t temp1 =
          work[7] + s1 + choice + kShaK[index] + words[index];
      const std::uint32_t s0 =
          rotr(work[0], 2) ^ rotr(work[0], 13) ^ rotr(work[0], 22);
      const std::uint32_t majority =
          (work[0] & work[1]) ^ (work[0] & work[2]) ^ (work[1] & work[2]);
      const std::uint32_t temp2 = s0 + majority;
      work[7] = work[6];
      work[6] = work[5];
      work[5] = work[4];
      work[4] = work[3] + temp1;
      work[3] = work[2];
      work[2] = work[1];
      work[1] = work[0];
      work[0] = temp1 + temp2;
    }
    for (std::size_t index = 0; index < state.size(); ++index)
      state[index] += work[index];
  }

  void update(std::string_view input) noexcept {
    for (const char raw_value : input) {
      block[used++] = static_cast<std::uint8_t>(
          static_cast<unsigned char>(raw_value));
      ++bytes;
      if (used == block.size()) {
        transform();
        used = 0;
      }
    }
  }

  std::array<std::uint8_t, 32> finish() noexcept {
    const std::uint64_t bit_count = bytes * 8U;
    block[used++] = 0x80U;
    if (used > 56) {
      while (used < block.size()) block[used++] = 0;
      transform();
      used = 0;
    }
    while (used < 56) block[used++] = 0;
    for (std::size_t index = 0; index < 8; ++index)
      block[63 - index] =
          static_cast<std::uint8_t>(bit_count >> (index * 8U));
    transform();
    std::array<std::uint8_t, 32> result{};
    for (std::size_t index = 0; index < state.size(); ++index)
      for (std::size_t byte = 0; byte < 4; ++byte)
        result[index * 4 + byte] =
            static_cast<std::uint8_t>(state[index] >> ((3 - byte) * 8U));
    return result;
  }
};

std::array<std::uint8_t, 32> sha256(std::string_view input) noexcept {
  Sha256 sha;
  sha.update(input);
  return sha.finish();
}

enum class JsonType : std::uint8_t { String, Number, Object, Other };
struct Field {
  std::string_view name{};
  std::string_view value{};
  JsonType type{JsonType::Other};
};
struct Fields {
  std::array<Field, 48> values{};
  std::size_t count{};

  const Field* find(std::string_view name) const noexcept {
    for (std::size_t index = 0; index < count; ++index)
      if (values[index].name == name) return &values[index];
    return nullptr;
  }
};

class JsonCursor {
 public:
  explicit JsonCursor(std::string_view input) : input_(input) {}

  ParseResult object(Fields& fields) noexcept {
    space();
    if (!take('{')) return ParseResult::Malformed;
    space();
    if (take('}')) return done() ? ParseResult::Ok : ParseResult::Malformed;
    while (position_ < input_.size()) {
      std::string_view name;
      if (!string(name)) return ParseResult::Malformed;
      for (std::size_t index = 0; index < fields.count; ++index)
        if (fields.values[index].name == name)
          return ParseResult::DuplicateField;
      if (fields.count == fields.values.size()) return ParseResult::Oversize;
      space();
      if (!take(':')) return ParseResult::Malformed;
      space();
      Field field{};
      field.name = name;
      const std::size_t start = position_;
      if (peek() == '"') {
        field.type = JsonType::String;
        if (!string(field.value)) return ParseResult::Malformed;
      } else if (peek() == '{') {
        field.type = JsonType::Object;
        if (!skip_composite('{', '}')) return ParseResult::Malformed;
        field.value = input_.substr(start, position_ - start);
      } else if (peek() == '[') {
        field.type = JsonType::Other;
        if (!skip_composite('[', ']')) return ParseResult::Malformed;
        field.value = input_.substr(start, position_ - start);
      } else {
        while (position_ < input_.size() && peek() != ',' && peek() != '}' &&
               peek() != ' ' && peek() != '\n' && peek() != '\r' &&
               peek() != '\t')
          ++position_;
        if (position_ == start) return ParseResult::Malformed;
        field.value = input_.substr(start, position_ - start);
        field.type =
            (field.value.front() == '-' ||
             (field.value.front() >= '0' && field.value.front() <= '9'))
                ? JsonType::Number
                : JsonType::Other;
      }
      fields.values[fields.count++] = field;
      space();
      if (take('}')) {
        space();
        return done() ? ParseResult::Ok : ParseResult::Malformed;
      }
      if (!take(',')) return ParseResult::Malformed;
      space();
    }
    return ParseResult::Malformed;
  }

 private:
  void space() noexcept {
    while (position_ < input_.size() &&
           (input_[position_] == ' ' || input_[position_] == '\n' ||
            input_[position_] == '\r' || input_[position_] == '\t'))
      ++position_;
  }
  bool take(char value) noexcept {
    if (position_ >= input_.size() || input_[position_] != value) return false;
    ++position_;
    return true;
  }
  char peek() const noexcept {
    return position_ < input_.size() ? input_[position_] : '\0';
  }
  bool done() const noexcept { return position_ == input_.size(); }
  bool string(std::string_view& output) noexcept {
    if (!take('"')) return false;
    const std::size_t start = position_;
    bool escaped = false;
    while (position_ < input_.size()) {
      const char value = input_[position_++];
      if (escaped) {
        escaped = false;
        continue;
      }
      if (value == '\\') {
        escaped = true;
        continue;
      }
      if (value == '"') {
        output = input_.substr(start, position_ - start - 1);
        return true;
      }
      if (static_cast<unsigned char>(value) < 0x20U) return false;
    }
    return false;
  }
  bool skip_composite(char open, char close) noexcept {
    std::size_t depth = 0;
    bool in_string = false;
    bool escaped = false;
    while (position_ < input_.size()) {
      const char value = input_[position_++];
      if (in_string) {
        if (escaped)
          escaped = false;
        else if (value == '\\')
          escaped = true;
        else if (value == '"')
          in_string = false;
        continue;
      }
      if (value == '"') {
        in_string = true;
      } else if (value == open) {
        ++depth;
      } else if (value == close) {
        if (depth == 0 || --depth == 0) return true;
      }
    }
    return false;
  }

  std::string_view input_;
  std::size_t position_{};
};

ParseResult fields(std::string_view json, Fields& output) noexcept {
  if (json.size() > kMaxResponseBytes) return ParseResult::Oversize;
  return JsonCursor(json).object(output);
}

bool as_u64(const Field* field, std::uint64_t& output) noexcept {
  if (field == nullptr || field->type != JsonType::Number ||
      field->value.empty() || field->value.front() == '-')
    return false;
  const auto result = std::from_chars(field->value.data(),
                                      field->value.data() + field->value.size(),
                                      output);
  return result.ec == std::errc{} &&
         result.ptr == field->value.data() + field->value.size();
}

bool as_i32(const Field* field, std::int32_t& output) noexcept {
  if (field == nullptr || field->type != JsonType::Number) return false;
  const auto result = std::from_chars(field->value.data(),
                                      field->value.data() + field->value.size(),
                                      output);
  return result.ec == std::errc{} &&
         result.ptr == field->value.data() + field->value.size();
}

template <typename Id>
bool copy_id(std::string_view value, Id& output) noexcept {
  if (value.empty() || value.size() > output.value.size()) return false;
  std::memcpy(output.value.data(), value.data(), value.size());
  output.length = static_cast<std::uint16_t>(value.size());
  return true;
}

template <typename Id>
bool copy_numeric_id(const Field* field, Id& output) noexcept {
  return field != nullptr && field->type == JsonType::Number &&
         copy_id(field->value, output);
}

bool decimal(std::string_view text, std::uint8_t scale,
             api::FixedPoint& output) noexcept {
  if (text.empty() || scale > 18) return false;
  bool negative = false;
  std::size_t index = 0;
  if (text[index] == '-') {
    negative = true;
    if (++index == text.size()) return false;
  }
  std::uint64_t value = 0;
  std::size_t fractional = 0;
  bool dot = false;
  bool digit = false;
  for (; index < text.size(); ++index) {
    const char character = text[index];
    if (character == '.') {
      if (dot || !digit) return false;
      dot = true;
      continue;
    }
    if (character < '0' || character > '9') return false;
    digit = true;
    if (dot && fractional >= scale) {
      if (character != '0') return false;
      continue;
    }
    const std::uint64_t number =
        static_cast<std::uint64_t>(character - '0');
    if (value > (static_cast<std::uint64_t>(
                     std::numeric_limits<std::int64_t>::max()) -
                 number) /
                    10U)
      return false;
    value = value * 10U + number;
    if (dot) ++fractional;
  }
  if (!digit || (dot && text.back() == '.')) return false;
  while (fractional++ < scale) {
    if (value > static_cast<std::uint64_t>(
                    std::numeric_limits<std::int64_t>::max() / 10))
      return false;
    value *= 10U;
  }
  output.value = negative ? -static_cast<std::int64_t>(value)
                          : static_cast<std::int64_t>(value);
  output.scale = scale;
  return true;
}

ParseResult common_ack(std::string_view json, const ParseContext& context,
                       api::VenueEventType type,
                       api::VenueEvent& output) noexcept {
  Fields object{};
  const ParseResult result = fields(json, object);
  if (result != ParseResult::Ok) return result;
  const Field* order_id = object.find("orderId");
  const Field* client_id = object.find("clientOrderId");
  if (order_id == nullptr || client_id == nullptr ||
      client_id->type != JsonType::String)
    return ParseResult::MissingField;
  output = {};
  output.type = type;
  output.handle = context.handle;
  output.token = context.token;
  if (!copy_numeric_id(order_id, output.venue_order_id) ||
      !copy_id(client_id->value, output.client_order_id))
    return ParseResult::InvalidValue;
  std::uint64_t time_ms = 0;
  const Field* time = object.find("transactTime");
  if (time == nullptr) time = object.find("updateTime");
  if (time != nullptr && !as_u64(time, time_ms)) return ParseResult::InvalidValue;
  if (time_ms > std::numeric_limits<std::uint64_t>::max() / 1000000ULL)
    return ParseResult::InvalidValue;
  output.event_time_ns = time_ms * 1000000ULL;
  return ParseResult::Ok;
}

api::OrderStatus order_status(std::string_view status) noexcept;

struct TradingParameter {
  std::string_view name{};
  std::string_view value{};
};

class TradingAppender {
 public:
  explicit TradingAppender(TradingRequest& request) : request_(request) {}

  bool append(std::string_view value) noexcept {
    if (value.size() > request_.payload.size() - size_) return false;
    std::memcpy(request_.payload.data() + size_, value.data(), value.size());
    size_ += value.size();
    return true;
  }
  bool quoted(std::string_view value) noexcept {
    if (!append("\"")) return false;
    for (const char character : value) {
      if (character == '"' || character == '\\') {
        const std::array<char, 2> escaped{'\\', character};
        if (!append({escaped.data(), escaped.size()})) return false;
      } else if (static_cast<unsigned char>(character) < 0x20U) {
        return false;
      } else if (!append({&character, 1})) {
        return false;
      }
    }
    return append("\"");
  }
  template <typename Integer>
  bool integer(Integer value) noexcept {
    std::array<char, 32> buffer{};
    const auto converted =
        std::to_chars(buffer.data(), buffer.data() + buffer.size(), value);
    return converted.ec == std::errc{} &&
           append({buffer.data(),
                   static_cast<std::size_t>(converted.ptr - buffer.data())});
  }
  void finish() noexcept {
    request_.payload_length = static_cast<std::uint16_t>(size_);
  }

 private:
  TradingRequest& request_;
  std::size_t size_{};
};

bool trading_numeric(std::string_view name) noexcept {
  return name == "timestamp" || name == "recvWindow" ||
         name == "goodTillDate" || name == "orderId";
}

bool build_trading_request(const HttpRequest& rest, std::string_view method,
                           std::uint32_t request_id,
                           CredentialsView credentials,
                           TradingRequest& output) noexcept {
  if (request_id == 0 || credentials.api_key.empty() ||
      credentials.secret_key.empty())
    return false;
  const std::string_view target = rest.target_view();
  const std::size_t query = target.find('?');
  if (query == std::string_view::npos) return false;
  std::array<TradingParameter, 16> parameters{};
  std::size_t count = 0;
  std::size_t position = query + 1;
  while (position < target.size()) {
    const std::size_t end = target.find('&', position);
    const std::string_view pair =
        target.substr(position, end == std::string_view::npos
                                    ? std::string_view::npos
                                    : end - position);
    const std::size_t equal = pair.find('=');
    if (equal == std::string_view::npos || equal == 0 ||
        equal + 1 == pair.size())
      return false;
    if (pair.substr(0, equal) != "signature") {
      if (count == parameters.size()) return false;
      parameters[count++] = {pair.substr(0, equal), pair.substr(equal + 1)};
    }
    if (end == std::string_view::npos) break;
    position = end + 1;
  }
  if (count == parameters.size()) return false;
  parameters[count++] = {"apiKey", credentials.api_key};
  std::sort(parameters.begin(), parameters.begin() + count,
            [](const TradingParameter& left,
               const TradingParameter& right) noexcept {
              return left.name < right.name;
            });

  std::array<char, kMaxRequestBytes> signing{};
  std::size_t signing_size = 0;
  for (std::size_t index = 0; index < count; ++index) {
    const auto append_signing = [&](std::string_view value) noexcept {
      if (value.size() > signing.size() - signing_size) return false;
      std::memcpy(signing.data() + signing_size, value.data(), value.size());
      signing_size += value.size();
      return true;
    };
    if ((index != 0 && !append_signing("&")) ||
        !append_signing(parameters[index].name) || !append_signing("=") ||
        !append_signing(parameters[index].value))
      return false;
  }
  std::array<char, 64> signature{};
  if (!hmac_sha256_hex(
          credentials.secret_key,
          {signing.data(), signing_size}, signature))
    return false;

  output = {};
  TradingAppender json(output);
  if (!json.append("{\"id\":") || !json.integer(request_id) ||
      !json.append(",\"method\":") || !json.quoted(method) ||
      !json.append(",\"params\":{"))
    return false;
  for (std::size_t index = 0; index < count; ++index) {
    if ((index != 0 && !json.append(",")) ||
        !json.quoted(parameters[index].name) || !json.append(":"))
      return false;
    if (parameters[index].value == "true" ||
        parameters[index].value == "false") {
      if (!json.append(parameters[index].value)) return false;
    } else if (trading_numeric(parameters[index].name)) {
      if (!json.append(parameters[index].value)) return false;
    } else if (!json.quoted(parameters[index].value)) {
      return false;
    }
  }
  if (!json.append(",\"signature\":") ||
      !json.quoted({signature.data(), signature.size()}) ||
      !json.append("}}"))
    return false;
  json.finish();
  return true;
}

void skip_space(std::string_view input, std::size_t& position) noexcept {
  while (position < input.size() &&
         (input[position] == ' ' || input[position] == '\n' ||
          input[position] == '\r' || input[position] == '\t'))
    ++position;
}

ParseResult next_array_object(std::string_view json, OpenOrdersCursor& cursor,
                              std::string_view& object) noexcept {
  std::size_t position = cursor.offset;
  skip_space(json, position);
  if (position == 0) {
    if (position >= json.size() || json[position++] != '[')
      return ParseResult::Malformed;
    skip_space(json, position);
  }
  if (position < json.size() && json[position] == ']') {
    ++position;
    skip_space(json, position);
    if (position != json.size()) return ParseResult::Malformed;
    cursor.offset = static_cast<std::uint32_t>(position);
    cursor.complete = true;
    object = {};
    return ParseResult::Ok;
  }
  if (position >= json.size() || json[position] != '{')
    return ParseResult::Malformed;
  const std::size_t start = position;
  std::size_t depth = 0;
  bool in_string = false;
  bool escaped = false;
  for (; position < json.size(); ++position) {
    const char value = json[position];
    if (in_string) {
      if (escaped)
        escaped = false;
      else if (value == '\\')
        escaped = true;
      else if (value == '"')
        in_string = false;
      continue;
    }
    if (value == '"') {
      in_string = true;
    } else if (value == '{') {
      ++depth;
    } else if (value == '}') {
      if (depth == 0) return ParseResult::Malformed;
      if (--depth == 0) {
        ++position;
        object = json.substr(start, position - start);
        skip_space(json, position);
        if (position >= json.size()) return ParseResult::Malformed;
        if (json[position] == ',') {
          ++position;
          skip_space(json, position);
          if (position >= json.size() || json[position] == ']')
            return ParseResult::Malformed;
        } else if (json[position] != ']') {
          return ParseResult::Malformed;
        }
        cursor.offset = static_cast<std::uint32_t>(position);
        return ParseResult::Ok;
      }
    }
  }
  return ParseResult::Malformed;
}

ParseResult open_order_event(std::string_view json,
                             api::VenueEvent& output) noexcept {
  Fields object{};
  const ParseResult result = fields(json, object);
  if (result != ParseResult::Ok) return result;
  const Field* status = object.find("status");
  const Field* client = object.find("clientOrderId");
  const Field* order = object.find("orderId");
  if (status == nullptr || client == nullptr || order == nullptr ||
      status->type != JsonType::String || client->type != JsonType::String)
    return ParseResult::MissingField;

  output = {};
  output.reconciled_status = order_status(status->value);
  if (output.reconciled_status == api::OrderStatus::Open ||
      output.reconciled_status == api::OrderStatus::PartiallyFilled) {
    output.type = api::VenueEventType::ReconcileOpen;
  } else if (output.reconciled_status == api::OrderStatus::Filled ||
             output.reconciled_status == api::OrderStatus::Canceled ||
             output.reconciled_status == api::OrderStatus::Rejected ||
             output.reconciled_status == api::OrderStatus::Expired) {
    output.type = api::VenueEventType::ReconcileTerminal;
  } else {
    return ParseResult::Unsupported;
  }
  if (!copy_id(client->value, output.client_order_id) ||
      !copy_numeric_id(order, output.venue_order_id))
    return ParseResult::InvalidValue;

  const Field* update_time = object.find("updateTime");
  if (update_time == nullptr) update_time = object.find("time");
  if (update_time != nullptr) {
    std::uint64_t time_ms = 0;
    if (!as_u64(update_time, time_ms) ||
        time_ms > std::numeric_limits<std::uint64_t>::max() / 1000000ULL)
      return ParseResult::InvalidValue;
    output.event_time_ns = time_ms * 1000000ULL;
  }
  return ParseResult::Ok;
}

api::VenueEventType event_type(std::string_view execution,
                               std::string_view status) noexcept {
  if (execution == "TRADE") return api::VenueEventType::Fill;
  if (execution == "CANCELED") return api::VenueEventType::CancelAck;
  if (execution == "REJECTED") return api::VenueEventType::NewReject;
  if (execution == "EXPIRED") return api::VenueEventType::Expire;
  if (execution == "NEW") return api::VenueEventType::NewAck;
  if (status == "CANCELED") return api::VenueEventType::CancelAck;
  if (status == "EXPIRED") return api::VenueEventType::Expire;
  return api::VenueEventType::ReconcileOpen;
}

api::OrderStatus order_status(std::string_view status) noexcept {
  if (status == "NEW") return api::OrderStatus::Open;
  if (status == "PARTIALLY_FILLED") return api::OrderStatus::PartiallyFilled;
  if (status == "FILLED") return api::OrderStatus::Filled;
  if (status == "CANCELED") return api::OrderStatus::Canceled;
  if (status == "REJECTED") return api::OrderStatus::Rejected;
  if (status == "EXPIRED") return api::OrderStatus::Expired;
  return api::OrderStatus::Unknown;
}

ParseResult stream_event(const Fields& object, std::string_view execution_name,
                         std::string_view status_name,
                         std::string_view client_name,
                         std::string_view order_name,
                         std::string_view last_qty_name,
                         std::string_view last_price_name,
                         std::string_view trade_name,
                         std::string_view event_time_name,
                         const ParseContext& context,
                         api::VenueEvent& output) noexcept {
  const Field* execution = object.find(execution_name);
  const Field* status = object.find(status_name);
  const Field* client = object.find(client_name);
  const Field* order = object.find(order_name);
  if (execution == nullptr || status == nullptr || client == nullptr ||
      order == nullptr || execution->type != JsonType::String ||
      status->type != JsonType::String || client->type != JsonType::String)
    return ParseResult::MissingField;
  output = {};
  output.type = event_type(execution->value, status->value);
  output.reconciled_status = order_status(status->value);
  output.handle = context.handle;
  output.token = context.token;
  if (!copy_id(client->value, output.client_order_id) ||
      !copy_numeric_id(order, output.venue_order_id))
    return ParseResult::InvalidValue;
  std::uint64_t event_ms = 0;
  if (!as_u64(object.find(event_time_name), event_ms) ||
      event_ms > std::numeric_limits<std::uint64_t>::max() / 1000000ULL)
    return ParseResult::InvalidValue;
  output.event_time_ns = event_ms * 1000000ULL;
  if (output.type == api::VenueEventType::Fill) {
    const Field* quantity = object.find(last_qty_name);
    const Field* price = object.find(last_price_name);
    const Field* trade = object.find(trade_name);
    if (quantity == nullptr || price == nullptr || trade == nullptr ||
        quantity->type != JsonType::String || price->type != JsonType::String ||
        !decimal(quantity->value, context.quantity_scale,
                 output.fill_quantity) ||
        !decimal(price->value, context.price_scale, output.fill_price) ||
        !copy_numeric_id(trade, output.trade_id))
      return ParseResult::InvalidValue;
  }
  return ParseResult::Ok;
}

}  // namespace

const Profile& profile(Product product) noexcept {
  return product == Product::Spot ? kSpotProfile : kUsdmProfile;
}

bool hmac_sha256_hex(std::string_view key, std::string_view message,
                     std::array<char, 64>& output) noexcept {
  std::array<std::uint8_t, 64> key_block{};
  if (key.size() > key_block.size()) {
    const auto digest = sha256(key);
    std::copy(digest.begin(), digest.end(), key_block.begin());
  } else {
    for (std::size_t index = 0; index < key.size(); ++index)
      key_block[index] = static_cast<std::uint8_t>(key[index]);
  }
  std::array<char, 64> inner_key{};
  std::array<char, 64> outer_key{};
  for (std::size_t index = 0; index < key_block.size(); ++index) {
    inner_key[index] = static_cast<char>(key_block[index] ^ 0x36U);
    outer_key[index] = static_cast<char>(key_block[index] ^ 0x5cU);
  }
  Sha256 inner;
  inner.update({inner_key.data(), inner_key.size()});
  inner.update(message);
  const auto inner_digest = inner.finish();
  Sha256 outer;
  outer.update({outer_key.data(), outer_key.size()});
  outer.update({reinterpret_cast<const char*>(inner_digest.data()),
                inner_digest.size()});
  const auto digest = outer.finish();
  constexpr char kHex[] = "0123456789abcdef";
  for (std::size_t index = 0; index < digest.size(); ++index) {
    output[index * 2] = kHex[digest[index] >> 4U];
    output[index * 2 + 1] = kHex[digest[index] & 0x0fU];
  }
  return true;
}

bool RequestBuilder::place(const PlaceParameters& parameters,
                           std::uint64_t timestamp_ms,
                           std::uint32_t recv_window_ms,
                           CredentialsView credentials,
                           HttpRequest& output) const noexcept {
  constexpr std::uint16_t kSupportedFlags =
      static_cast<std::uint16_t>(
          api::OrderFlag::PostOnly | api::OrderFlag::ReduceOnly |
          api::OrderFlag::ClosePosition | api::OrderFlag::QuoteQuantity);
  if (!valid_token(parameters.symbol) ||
      !valid_token(parameters.client_order_id) ||
      parameters.quantity.value <= 0 || parameters.price.value < 0 ||
      (parameters.side != api::Side::Buy &&
       parameters.side != api::Side::Sell) ||
      (parameters.type != api::OrderType::Limit &&
       parameters.type != api::OrderType::Market) ||
      (parameters.flags & static_cast<std::uint16_t>(~kSupportedFlags)) != 0)
    return false;
  // The OMS order model does not expose Binance's stop order types, the only
  // USD-M order types for which closePosition is valid. Reject instead of
  // silently sending a quantity order with different semantics.
  if ((parameters.flags & api::OrderFlag::ClosePosition) != 0) return false;
  if ((parameters.flags & api::OrderFlag::QuoteQuantity) != 0 &&
      (product_ == Product::Usdm ||
       parameters.type != api::OrderType::Market))
    return false;
  if ((parameters.flags &
       static_cast<std::uint16_t>(api::OrderFlag::ReduceOnly |
                                  api::OrderFlag::ClosePosition)) != 0 &&
      product_ == Product::Spot)
    return false;
  const bool post_only = (parameters.flags & api::OrderFlag::PostOnly) != 0;
  if (post_only && parameters.type != api::OrderType::Limit) return false;
  if (parameters.type == api::OrderType::Limit &&
      parameters.time_in_force != api::TimeInForce::GTC &&
      parameters.time_in_force != api::TimeInForce::GTD &&
      parameters.time_in_force != api::TimeInForce::IOC &&
      parameters.time_in_force != api::TimeInForce::FOK)
    return false;
  initialize_request(HttpMethod::Post, credentials, true, output);
  Appender appender(output);
  const std::string_view type =
      post_only && product_ == Product::Spot
          ? "LIMIT_MAKER"
          : (parameters.type == api::OrderType::Limit ? "LIMIT" : "MARKET");
  if (!appender.append(profile(product_).place_path) ||
      !appender.append("?symbol=") || !appender.encoded(parameters.symbol) ||
      !appender.append("&side=") || !appender.append(side_name(parameters.side)) ||
      !appender.append("&type=") || !appender.append(type))
    return false;
  if (parameters.type == api::OrderType::Limit && !post_only) {
    const std::string_view tif = tif_name(parameters.time_in_force);
    if (tif.empty() || (parameters.time_in_force == api::TimeInForce::GTD &&
                        product_ != Product::Usdm) ||
        !appender.append("&timeInForce=") || !appender.append(tif))
      return false;
  }
  const bool quote_quantity =
      (parameters.flags & api::OrderFlag::QuoteQuantity) != 0;
  if (!appender.append(quote_quantity ? "&quoteOrderQty=" : "&quantity=") ||
      !appender.fixed(parameters.quantity))
    return false;
  if (parameters.type == api::OrderType::Limit &&
      (!appender.append("&price=") || !appender.fixed(parameters.price)))
    return false;
  if (!appender.append("&newClientOrderId=") ||
      !appender.encoded(parameters.client_order_id))
    return false;
  if (product_ == Product::Usdm &&
      (parameters.flags & api::OrderFlag::ReduceOnly) != 0 &&
      !appender.append("&reduceOnly=true"))
    return false;
  if (product_ == Product::Usdm && post_only &&
      !appender.append("&timeInForce=GTX"))
    return false;
  if (parameters.time_in_force == api::TimeInForce::GTD) {
    const std::uint64_t expiry_seconds =
        parameters.expire_time_ns / 1000000000ULL;
    const std::uint64_t timestamp_seconds = timestamp_ms / 1000ULL;
    if (expiry_seconds <= timestamp_seconds ||
        expiry_seconds - timestamp_seconds <= 600ULL ||
        !appender.append("&goodTillDate=") ||
        !appender.integer(expiry_seconds))
      return false;
  }
  return append_signed_tail(appender, timestamp_ms, recv_window_ms, credentials,
                            output);
}

bool RequestBuilder::cancel(const CancelParameters& parameters,
                            std::uint64_t timestamp_ms,
                            std::uint32_t recv_window_ms,
                            CredentialsView credentials,
                            HttpRequest& output) const noexcept {
  if (!valid_token(parameters.symbol) ||
      (parameters.client_order_id.empty() && parameters.venue_order_id.empty()) ||
      (!parameters.client_order_id.empty() &&
       !valid_token(parameters.client_order_id)))
    return false;
  initialize_request(HttpMethod::Delete, credentials, true, output);
  Appender appender(output);
  if (!appender.append(profile(product_).cancel_path) ||
      !appender.append("?symbol=") || !appender.encoded(parameters.symbol))
    return false;
  if (!parameters.venue_order_id.empty()) {
    for (const char character : parameters.venue_order_id)
      if (character < '0' || character > '9') return false;
    if (!appender.append("&orderId=") ||
        !appender.append(parameters.venue_order_id))
      return false;
  } else if (!appender.append("&origClientOrderId=") ||
             !appender.encoded(parameters.client_order_id)) {
    return false;
  }
  return append_signed_tail(appender, timestamp_ms, recv_window_ms, credentials,
                            output);
}

bool RequestBuilder::trading_place(const PlaceParameters& parameters,
                                   std::uint32_t request_id,
                                   std::uint64_t timestamp_ms,
                                   std::uint32_t recv_window_ms,
                                   CredentialsView credentials,
                                   TradingRequest& output) const noexcept {
  HttpRequest validated{};
  return place(parameters, timestamp_ms, recv_window_ms, credentials,
               validated) &&
         build_trading_request(validated, "order.place", request_id,
                               credentials, output);
}

bool RequestBuilder::trading_cancel(const CancelParameters& parameters,
                                    std::uint32_t request_id,
                                    std::uint64_t timestamp_ms,
                                    std::uint32_t recv_window_ms,
                                    CredentialsView credentials,
                                    TradingRequest& output) const noexcept {
  HttpRequest validated{};
  return cancel(parameters, timestamp_ms, recv_window_ms, credentials,
                validated) &&
         build_trading_request(validated, "order.cancel", request_id,
                               credentials, output);
}

bool RequestBuilder::open_orders(std::string_view symbol,
                                 std::uint64_t timestamp_ms,
                                 std::uint32_t recv_window_ms,
                                 CredentialsView credentials,
                                 HttpRequest& output) const noexcept {
  if (!symbol.empty() && !valid_token(symbol)) return false;
  initialize_request(HttpMethod::Get, credentials, true, output);
  Appender appender(output);
  if (!appender.append(profile(product_).open_orders_path))
    return false;
  if (!symbol.empty()) {
    if (!appender.append("?symbol=") || !appender.encoded(symbol))
      return false;
    return append_signed_tail(appender, timestamp_ms, recv_window_ms,
                              credentials, output);
  }
  if (credentials.api_key.empty() || credentials.secret_key.empty() ||
      recv_window_ms == 0 || recv_window_ms > 60000 ||
      !appender.append("?timestamp=") || !appender.integer(timestamp_ms) ||
      !appender.append("&recvWindow=") ||
      !appender.integer(recv_window_ms))
    return false;
  std::array<char, 64> signature{};
  const std::string_view target{output.target.data(), appender.size()};
  const std::size_t query = target.find('?');
  if (query == std::string_view::npos ||
      !hmac_sha256_hex(credentials.secret_key, target.substr(query + 1),
                       signature) ||
      !appender.append("&signature=") ||
      !appender.append({signature.data(), signature.size()}))
    return false;
  appender.finish();
  return true;
}

bool RequestBuilder::query_order(const CancelParameters& parameters,
                                 std::uint64_t timestamp_ms,
                                 std::uint32_t recv_window_ms,
                                 CredentialsView credentials,
                                 HttpRequest& output) const noexcept {
  if (!valid_token(parameters.symbol) ||
      (parameters.client_order_id.empty() &&
       parameters.venue_order_id.empty()) ||
      (!parameters.client_order_id.empty() &&
       !valid_token(parameters.client_order_id)))
    return false;
  initialize_request(HttpMethod::Get, credentials, true, output);
  Appender appender(output);
  if (!appender.append(profile(product_).place_path) ||
      !appender.append("?symbol=") ||
      !appender.encoded(parameters.symbol))
    return false;
  if (!parameters.client_order_id.empty()) {
    if (!appender.append("&origClientOrderId=") ||
        !appender.encoded(parameters.client_order_id))
      return false;
  } else {
    for (const char character : parameters.venue_order_id)
      if (character < '0' || character > '9') return false;
    if (!appender.append("&orderId=") ||
        !appender.append(parameters.venue_order_id))
      return false;
  }
  return append_signed_tail(appender, timestamp_ms, recv_window_ms,
                            credentials, output);
}

bool RequestBuilder::create_listen_key(CredentialsView credentials,
                                       HttpRequest& output) const noexcept {
  if (credentials.api_key.empty()) return false;
  initialize_request(HttpMethod::Post, credentials, false, output);
  Appender appender(output);
  if (!appender.append(profile(product_).listen_key_path)) return false;
  appender.finish();
  return true;
}

bool RequestBuilder::keepalive_listen_key(
    CredentialsView credentials, std::string_view listen_key,
    HttpRequest& output) const noexcept {
  if (credentials.api_key.empty() ||
      (product_ == Product::Spot && !valid_token(listen_key)))
    return false;
  initialize_request(HttpMethod::Put, credentials, false, output);
  Appender appender(output);
  if (!appender.append(profile(product_).listen_key_path)) return false;
  if (product_ == Product::Spot &&
      (!appender.append("?listenKey=") || !appender.encoded(listen_key)))
    return false;
  appender.finish();
  return true;
}

bool RequestBuilder::close_listen_key(CredentialsView credentials,
                                      std::string_view listen_key,
                                      HttpRequest& output) const noexcept {
  if (credentials.api_key.empty() ||
      (product_ == Product::Spot && !valid_token(listen_key)))
    return false;
  initialize_request(HttpMethod::Delete, credentials, false, output);
  Appender appender(output);
  if (!appender.append(profile(product_).listen_key_path)) return false;
  if (product_ == Product::Spot &&
      (!appender.append("?listenKey=") || !appender.encoded(listen_key)))
    return false;
  appender.finish();
  return true;
}

bool RequestBuilder::server_time(HttpRequest& output) const noexcept {
  output = {};
  output.method = HttpMethod::Get;
  Appender appender(output);
  if (!appender.append(profile(product_).time_path)) return false;
  appender.finish();
  return true;
}

ParseResult parse_rest_place(std::string_view json,
                             const ParseContext& context,
                             api::VenueEvent& output) noexcept {
  return common_ack(json, context, api::VenueEventType::NewAck, output);
}

ParseResult parse_rest_cancel(std::string_view json,
                              const ParseContext& context,
                              api::VenueEvent& output) noexcept {
  return common_ack(json, context, api::VenueEventType::CancelAck, output);
}

ParseResult parse_trading_response(std::string_view json,
                                   std::uint32_t& request_id,
                                   std::uint32_t& status,
                                   std::string_view& payload) noexcept {
  request_id = 0;
  status = 0;
  payload = {};
  Fields object{};
  const ParseResult parsed = fields(json, object);
  if (parsed != ParseResult::Ok) return parsed;
  std::uint64_t id = 0;
  std::uint64_t status_value = 0;
  if (!as_u64(object.find("id"), id) ||
      id == 0 || id > std::numeric_limits<std::uint32_t>::max() ||
      !as_u64(object.find("status"), status_value) ||
      status_value > 999)
    return ParseResult::InvalidValue;
  const Field* body =
      status_value < 400 ? object.find("result") : object.find("error");
  if (body == nullptr || body->type != JsonType::Object)
    return ParseResult::MissingField;
  request_id = static_cast<std::uint32_t>(id);
  status = static_cast<std::uint32_t>(status_value);
  payload = body->value;
  return ParseResult::Ok;
}

ParseResult parse_open_orders_page(std::string_view json,
                                   OpenOrdersCursor& cursor,
                                   api::VenueEvent* output,
                                   std::size_t capacity,
                                   std::size_t& output_count) noexcept {
  output_count = 0;
  if (json.size() > kMaxResponseBytes) return ParseResult::Oversize;
  if (cursor.complete) return ParseResult::Ok;
  if (output == nullptr || capacity == 0 ||
      cursor.page_count >= kMaxOpenOrderPages)
    return ParseResult::Oversize;
  ++cursor.page_count;
  while (output_count < capacity && !cursor.complete) {
    if (cursor.order_count >= kMaxOpenOrders) return ParseResult::Oversize;
    std::string_view object;
    const ParseResult next = next_array_object(json, cursor, object);
    if (next != ParseResult::Ok) return next;
    if (cursor.complete) break;
    const ParseResult parsed = open_order_event(object, output[output_count]);
    if (parsed != ParseResult::Ok) return parsed;
    ++output_count;
    ++cursor.order_count;
  }
  return ParseResult::Ok;
}

ParseResult parse_order_query(std::string_view json,
                              api::VenueEvent& output) noexcept {
  if (json.size() > kMaxResponseBytes) return ParseResult::Oversize;
  return open_order_event(json, output);
}

ParseResult parse_error(std::string_view json, std::uint32_t http_status,
                        RateLimitMetadata metadata,
                        VenueError& output) noexcept {
  output = {};
  Fields object{};
  const ParseResult result = fields(json, object);
  if (result != ParseResult::Ok) return result;
  const Field* code = object.find("code");
  const Field* message = object.find("msg");
  if (!as_i32(code, output.code) || message == nullptr ||
      message->type != JsonType::String)
    return ParseResult::MissingField;
  if (message->value.size() > output.message.size())
    return ParseResult::Oversize;
  std::memcpy(output.message.data(), message->value.data(),
              message->value.size());
  output.message_length =
      static_cast<std::uint16_t>(message->value.size());
  metadata.http_status = http_status;
  output.rate_limit = metadata;
  output.classification = classify_error(output.code, http_status);
  return ParseResult::Ok;
}

ParseResult parse_spot_execution_report(std::string_view json,
                                        const ParseContext& context,
                                        api::VenueEvent& output) noexcept {
  Fields object{};
  const ParseResult result = fields(json, object);
  if (result != ParseResult::Ok) return result;
  const Field* event = object.find("e");
  if (event == nullptr || event->type != JsonType::String ||
      event->value != "executionReport")
    return ParseResult::Unsupported;
  return stream_event(object, "x", "X", "c", "i", "l", "L", "t", "E",
                      context, output);
}

ParseResult parse_usdm_order_trade_update(std::string_view json,
                                          const ParseContext& context,
                                          api::VenueEvent& output) noexcept {
  Fields root{};
  const ParseResult result = fields(json, root);
  if (result != ParseResult::Ok) return result;
  const Field* event = root.find("e");
  const Field* order = root.find("o");
  if (event == nullptr || event->type != JsonType::String ||
      event->value != "ORDER_TRADE_UPDATE" || order == nullptr ||
      order->type != JsonType::Object)
    return ParseResult::Unsupported;
  Fields nested{};
  const ParseResult nested_result = fields(order->value, nested);
  if (nested_result != ParseResult::Ok) return nested_result;
  const ParseResult parsed =
      stream_event(nested, "x", "X", "c", "i", "l", "L", "t", "T",
                   context, output);
  if (parsed != ParseResult::Ok) return parsed;
  std::uint64_t event_ms = 0;
  if (as_u64(root.find("E"), event_ms) &&
      event_ms <= std::numeric_limits<std::uint64_t>::max() / 1000000ULL)
    output.event_time_ns = event_ms * 1000000ULL;
  return ParseResult::Ok;
}

ParseResult parse_listen_key(
    std::string_view json, std::array<char, kMaxListenKeyBytes>& output,
    std::uint16_t& output_length) noexcept {
  Fields object{};
  const ParseResult result = fields(json, object);
  if (result != ParseResult::Ok) return result;
  const Field* key = object.find("listenKey");
  if (key == nullptr || key->type != JsonType::String)
    return ParseResult::MissingField;
  if (key->value.empty() || key->value.size() > output.size())
    return ParseResult::InvalidValue;
  std::memcpy(output.data(), key->value.data(), key->value.size());
  output_length = static_cast<std::uint16_t>(key->value.size());
  return ParseResult::Ok;
}

ParseResult parse_server_time(std::string_view json,
                              std::uint64_t& output_ms) noexcept {
  Fields object{};
  const ParseResult result = fields(json, object);
  if (result != ParseResult::Ok) return result;
  return as_u64(object.find("serverTime"), output_ms)
             ? ParseResult::Ok
             : ParseResult::InvalidValue;
}

void ServerClock::observe(std::uint64_t local_send_ms,
                          std::uint64_t local_receive_ms,
                          std::uint64_t server_ms) noexcept {
  if (local_receive_ms < local_send_ms) return;
  const std::uint64_t midpoint =
      local_send_ms + (local_receive_ms - local_send_ms) / 2U;
  const std::int64_t sample =
      server_ms >= midpoint
          ? static_cast<std::int64_t>(std::min<std::uint64_t>(
                server_ms - midpoint,
                static_cast<std::uint64_t>(
                    std::numeric_limits<std::int64_t>::max())))
          : -static_cast<std::int64_t>(std::min<std::uint64_t>(
                midpoint - server_ms,
                static_cast<std::uint64_t>(
                    std::numeric_limits<std::int64_t>::max())));
  offset_ms_ = synchronized_ ? (offset_ms_ * 3 + sample) / 4 : sample;
  synchronized_ = true;
}

std::uint64_t ServerClock::venue_time_ms(std::uint64_t local_ms) const noexcept {
  if (offset_ms_ >= 0) {
    const auto offset = static_cast<std::uint64_t>(offset_ms_);
    return local_ms > std::numeric_limits<std::uint64_t>::max() - offset
               ? std::numeric_limits<std::uint64_t>::max()
               : local_ms + offset;
  }
  const auto offset = static_cast<std::uint64_t>(-(offset_ms_ + 1)) + 1U;
  return local_ms > offset ? local_ms - offset : 0;
}

ListenKeyAction ListenKeySession::start() noexcept {
  if (state_ != ListenKeyState::Empty &&
      state_ != ListenKeyState::RecreateRequired)
    return ListenKeyAction::None;
  state_ = ListenKeyState::Creating;
  return ListenKeyAction::Create;
}

bool ListenKeySession::activated(std::string_view key,
                                 std::uint64_t now_ns) noexcept {
  if (state_ != ListenKeyState::Creating || key.empty() ||
      key.size() > key_.size())
    return false;
  std::memcpy(key_.data(), key.data(), key.size());
  key_length_ = static_cast<std::uint16_t>(key.size());
  activated_ns_ = now_ns;
  next_deadline_ns_ =
      now_ns > std::numeric_limits<std::uint64_t>::max() - kKeepaliveIntervalNs
          ? std::numeric_limits<std::uint64_t>::max()
          : now_ns + kKeepaliveIntervalNs;
  state_ = ListenKeyState::Active;
  return true;
}

ListenKeyAction ListenKeySession::on_deadline(std::uint64_t now_ns) noexcept {
  if (state_ != ListenKeyState::Active || now_ns < next_deadline_ns_)
    return ListenKeyAction::None;
  if (now_ns >= activated_ns_ && now_ns - activated_ns_ >= kExpiryNs) {
    state_ = ListenKeyState::RecreateRequired;
    return ListenKeyAction::ReconnectStream;
  }
  state_ = ListenKeyState::KeepalivePending;
  return ListenKeyAction::Keepalive;
}

void ListenKeySession::keepalive_succeeded(std::uint64_t now_ns) noexcept {
  if (state_ != ListenKeyState::KeepalivePending) return;
  activated_ns_ = now_ns;
  next_deadline_ns_ =
      now_ns > std::numeric_limits<std::uint64_t>::max() - kKeepaliveIntervalNs
          ? std::numeric_limits<std::uint64_t>::max()
          : now_ns + kKeepaliveIntervalNs;
  state_ = ListenKeyState::Active;
}

void ListenKeySession::request_failed() noexcept {
  if (state_ == ListenKeyState::Creating ||
      state_ == ListenKeyState::KeepalivePending)
    state_ = ListenKeyState::RecreateRequired;
}

ListenKeyAction ListenKeySession::shutdown() noexcept {
  if (state_ == ListenKeyState::Empty || state_ == ListenKeyState::Closing)
    return ListenKeyAction::None;
  state_ = ListenKeyState::Closing;
  next_deadline_ns_ = 0;
  return key_length_ == 0 ? ListenKeyAction::None : ListenKeyAction::Close;
}

}  // namespace oms::exchange::binance

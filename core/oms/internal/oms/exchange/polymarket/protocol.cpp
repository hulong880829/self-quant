#include "oms/exchange/polymarket/protocol.h"

#include <algorithm>
#include <array>
#include <charconv>
#include <cctype>
#include <cstring>
#include <limits>

namespace oms::exchange::polymarket {
namespace {

template <std::size_t Capacity>
class Writer {
 public:
  explicit Writer(std::array<char, Capacity>& data) : data_(data) {}

  bool append(std::string_view value) noexcept {
    if (value.size() > data_.size() - size_) return false;
    std::copy(value.begin(), value.end(),
              data_.begin() + static_cast<std::ptrdiff_t>(size_));
    size_ += value.size();
    return true;
  }

  bool append(char value) noexcept {
    if (size_ == data_.size()) return false;
    data_[size_++] = value;
    return true;
  }

  bool decimal(std::uint64_t value) noexcept {
    std::array<char, 24> buffer{};
    const auto result =
        std::to_chars(buffer.data(), buffer.data() + buffer.size(), value);
    return result.ec == std::errc{} &&
           append(std::string_view(buffer.data(),
                                   static_cast<std::size_t>(result.ptr -
                                                            buffer.data())));
  }

  bool hex(const std::uint8_t* value, std::size_t count) noexcept {
    static constexpr char digits[] = "0123456789abcdef";
    if (count > (data_.size() - size_) / 2) return false;
    for (std::size_t index = 0; index < count; ++index) {
      data_[size_++] = digits[value[index] >> 4U];
      data_[size_++] = digits[value[index] & 0x0fU];
    }
    return true;
  }

  bool json_string(std::string_view value) noexcept {
    if (!append('"')) return false;
    for (const char item : value) {
      if (static_cast<unsigned char>(item) < 0x20U) return false;
      if ((item == '"' || item == '\\') && !append('\\')) return false;
      if (!append(item)) return false;
    }
    return append('"');
  }

  [[nodiscard]] std::size_t size() const noexcept { return size_; }

 private:
  std::array<char, Capacity>& data_;
  std::size_t size_{};
};

bool SetMethodPath(WireRequest& request, std::string_view method,
                   std::string_view path) noexcept {
  if (method.size() > request.method.size() ||
      path.size() > request.path.size())
    return false;
  std::copy(method.begin(), method.end(), request.method.begin());
  std::copy(path.begin(), path.end(), request.path.begin());
  request.method_size = static_cast<std::uint8_t>(method.size());
  request.path_size = static_cast<std::uint16_t>(path.size());
  return true;
}

std::string_view Trim(std::string_view value) noexcept {
  while (!value.empty() &&
         std::isspace(static_cast<unsigned char>(value.front())) != 0)
    value.remove_prefix(1);
  while (!value.empty() &&
         std::isspace(static_cast<unsigned char>(value.back())) != 0)
    value.remove_suffix(1);
  return value;
}

bool JsonString(std::string_view object, std::string_view key,
                std::string_view& output) noexcept {
  std::array<char, 96> pattern{};
  if (key.size() + 2 > pattern.size()) return false;
  pattern[0] = '"';
  std::copy(key.begin(), key.end(), pattern.begin() + 1);
  pattern[key.size() + 1] = '"';
  const std::string_view needle(pattern.data(), key.size() + 2);
  std::size_t position = object.find(needle);
  if (position == std::string_view::npos) return false;
  position = object.find(':', position + needle.size());
  if (position == std::string_view::npos) return false;
  ++position;
  while (position < object.size() &&
         std::isspace(static_cast<unsigned char>(object[position])) != 0)
    ++position;
  if (position >= object.size() || object[position] != '"') return false;
  const std::size_t start = ++position;
  bool escaped = false;
  for (; position < object.size(); ++position) {
    if (!escaped && object[position] == '"') {
      output = object.substr(start, position - start);
      return output.find('\\') == std::string_view::npos;
    }
    escaped = !escaped && object[position] == '\\';
    if (object[position] != '\\') escaped = false;
  }
  return false;
}

bool JsonScalar(std::string_view object, std::string_view key,
                std::string_view& output) noexcept {
  if (JsonString(object, key, output)) return true;
  std::array<char, 96> pattern{};
  if (key.size() + 2 > pattern.size()) return false;
  pattern[0] = '"';
  std::copy(key.begin(), key.end(), pattern.begin() + 1);
  pattern[key.size() + 1] = '"';
  const std::string_view needle(pattern.data(), key.size() + 2);
  std::size_t position = object.find(needle);
  if (position == std::string_view::npos) return false;
  position = object.find(':', position + needle.size());
  if (position == std::string_view::npos) return false;
  std::size_t begin = position + 1;
  while (begin < object.size() &&
         std::isspace(static_cast<unsigned char>(object[begin])) != 0)
    ++begin;
  std::size_t end = begin;
  while (end < object.size() && object[end] != ',' && object[end] != '}' &&
         std::isspace(static_cast<unsigned char>(object[end])) == 0)
    ++end;
  output = object.substr(begin, end - begin);
  return !output.empty();
}

bool JsonBool(std::string_view object, std::string_view key,
              bool& output) noexcept {
  std::array<char, 96> pattern{};
  if (key.size() + 2 > pattern.size()) return false;
  pattern[0] = '"';
  std::copy(key.begin(), key.end(), pattern.begin() + 1);
  pattern[key.size() + 1] = '"';
  const std::string_view quoted(pattern.data(), key.size() + 2);
  std::size_t position = object.find(quoted);
  if (position == std::string_view::npos) return false;
  position = object.find(':', position + quoted.size());
  if (position == std::string_view::npos) return false;
  const std::string_view tail = Trim(object.substr(position + 1));
  if (tail.starts_with("true")) {
    output = true;
    return true;
  }
  if (tail.starts_with("false")) {
    output = false;
    return true;
  }
  return false;
}

bool CopyId(std::string_view value, auto& output) noexcept {
  if (value.empty() || value.size() > output.value.size()) return false;
  std::copy(value.begin(), value.end(), output.value.begin());
  output.length = static_cast<std::uint16_t>(value.size());
  return true;
}

bool ParseFixed(std::string_view text, api::FixedPoint& output) noexcept {
  text = Trim(text);
  if (text.empty() || text.front() == '-') return false;
  std::uint64_t value = 0;
  std::uint8_t scale = 0;
  bool dot = false;
  bool digit = false;
  for (const char item : text) {
    if (item == '.' && !dot) {
      dot = true;
      continue;
    }
    if (item < '0' || item > '9') return false;
    digit = true;
    if (value > (static_cast<std::uint64_t>(
                     std::numeric_limits<std::int64_t>::max()) -
                 static_cast<unsigned>(item - '0')) /
                    10U)
      return false;
    value = value * 10U + static_cast<unsigned>(item - '0');
    if (dot && scale == 18) return false;
    if (dot) ++scale;
  }
  if (!digit) return false;
  output.value = static_cast<std::int64_t>(value);
  output.scale = scale;
  return true;
}

bool ParseTimeNs(std::string_view text, std::uint64_t& output) noexcept {
  text = Trim(text);
  const std::size_t decimal = text.find('.');
  const std::string_view seconds_text = text.substr(0, decimal);
  if (seconds_text.empty()) return false;
  std::uint64_t fraction_ns = 0;
  if (decimal != std::string_view::npos) {
    const std::string_view fraction = text.substr(decimal + 1);
    if (fraction.empty() || fraction.size() > 9 ||
        !std::all_of(fraction.begin(), fraction.end(),
                     [](char value) { return value >= '0' && value <= '9'; }))
      return false;
    for (const char value : fraction)
      fraction_ns = fraction_ns * 10U + static_cast<unsigned>(value - '0');
    for (std::size_t index = fraction.size(); index < 9; ++index)
      fraction_ns *= 10U;
  }
  std::uint64_t seconds = 0;
  const auto result = std::from_chars(seconds_text.data(),
                                      seconds_text.data() + seconds_text.size(),
                                      seconds);
  if (result.ec != std::errc{} ||
      result.ptr != seconds_text.data() + seconds_text.size() ||
      seconds > (std::numeric_limits<std::uint64_t>::max() - fraction_ns) /
                    1000000000ULL)
    return false;
  output = seconds * 1000000000ULL + fraction_ns;
  return true;
}

bool EqualFold(std::string_view left, std::string_view right) noexcept {
  return left.size() == right.size() &&
         std::equal(left.begin(), left.end(), right.begin(),
                    [](char lhs, char rhs) {
                      return std::toupper(static_cast<unsigned char>(lhs)) ==
                             std::toupper(static_cast<unsigned char>(rhs));
                    });
}

bool ArrayRange(std::string_view json, std::string_view& array) noexcept {
  std::size_t begin = 0;
  const std::string_view trimmed = Trim(json);
  if (trimmed.starts_with('[')) {
    begin = static_cast<std::size_t>(trimmed.data() - json.data());
  } else {
    const std::size_t data = json.find("\"data\"");
    if (data == std::string_view::npos) return false;
    begin = json.find('[', data + 6);
    if (begin == std::string_view::npos) return false;
  }
  bool string = false;
  bool escaped = false;
  unsigned depth = 0;
  for (std::size_t index = begin; index < json.size(); ++index) {
    const char item = json[index];
    if (string) {
      if (!escaped && item == '"') string = false;
      escaped = !escaped && item == '\\';
      if (item != '\\') escaped = false;
      continue;
    }
    if (item == '"') {
      string = true;
    } else if (item == '[') {
      ++depth;
    } else if (item == ']' && --depth == 0) {
      array = json.substr(begin + 1, index - begin - 1);
      return true;
    }
  }
  return false;
}

bool ArrayFieldRange(std::string_view json, std::string_view key,
                     std::string_view& array) noexcept {
  std::array<char, 96> pattern{};
  if (key.size() + 2 > pattern.size()) return false;
  pattern[0] = '"';
  std::copy(key.begin(), key.end(), pattern.begin() + 1);
  pattern[key.size() + 1] = '"';
  const std::string_view needle(pattern.data(), key.size() + 2);
  const std::size_t field = json.find(needle);
  if (field == std::string_view::npos) return false;
  const std::size_t colon = json.find(':', field + needle.size());
  const std::size_t begin =
      colon == std::string_view::npos ? colon : json.find('[', colon + 1);
  if (begin == std::string_view::npos) return false;
  bool string = false;
  bool escaped = false;
  unsigned depth = 0;
  for (std::size_t index = begin; index < json.size(); ++index) {
    const char item = json[index];
    if (string) {
      if (!escaped && item == '"') string = false;
      escaped = !escaped && item == '\\';
      if (item != '\\') escaped = false;
      continue;
    }
    if (item == '"') {
      string = true;
    } else if (item == '[') {
      ++depth;
    } else if (item == ']' && --depth == 0) {
      array = json.substr(begin + 1, index - begin - 1);
      return true;
    }
  }
  return false;
}

bool NextObject(std::string_view array, std::size_t& offset,
                std::string_view& object, bool& malformed) noexcept {
  malformed = false;
  const std::size_t begin = array.find('{', offset);
  if (begin == std::string_view::npos) return false;
  unsigned depth = 0;
  bool string = false;
  bool escaped = false;
  for (std::size_t index = begin; index < array.size(); ++index) {
    const char item = array[index];
    if (string) {
      if (!escaped && item == '"') string = false;
      escaped = !escaped && item == '\\';
      if (item != '\\') escaped = false;
      continue;
    }
    if (item == '"') {
      string = true;
    } else if (item == '{') {
      ++depth;
    } else if (item == '}' && --depth == 0) {
      object = array.substr(begin, index - begin + 1);
      offset = index + 1;
      return true;
    }
  }
  offset = array.size();
  malformed = true;
  return false;
}

std::uint64_t Power10(std::uint8_t exponent) noexcept {
  std::uint64_t value = 1;
  while (exponent-- != 0) value *= 10U;
  return value;
}

int CompareFixed(api::FixedPoint left, api::FixedPoint right) noexcept {
  const auto left_value = static_cast<std::uint64_t>(left.value);
  const auto right_value = static_cast<std::uint64_t>(right.value);
  if (left.scale == right.scale)
    return left_value < right_value ? -1
                                    : (left_value > right_value ? 1 : 0);
  if (left.scale < right.scale) {
    const std::uint64_t multiplier =
        Power10(static_cast<std::uint8_t>(right.scale - left.scale));
    if (left_value > std::numeric_limits<std::uint64_t>::max() / multiplier)
      return 1;
    const std::uint64_t scaled = left_value * multiplier;
    return scaled < right_value ? -1 : (scaled > right_value ? 1 : 0);
  }
  const std::uint64_t multiplier =
      Power10(static_cast<std::uint8_t>(left.scale - right.scale));
  if (right_value > std::numeric_limits<std::uint64_t>::max() / multiplier)
    return -1;
  const std::uint64_t scaled = right_value * multiplier;
  return left_value < scaled ? -1 : (left_value > scaled ? 1 : 0);
}

std::uint64_t GreatestCommonDivisor(std::uint64_t left,
                                    std::uint64_t right) noexcept {
  while (right != 0) {
    const std::uint64_t remainder = left % right;
    left = right;
    right = remainder;
  }
  return left;
}

bool AppendQueryEncoded(Writer<512>& writer, std::string_view value) noexcept {
  static constexpr char digits[] = "0123456789ABCDEF";
  for (const char character : value) {
    const auto item = static_cast<unsigned char>(character);
    const bool unreserved =
        (item >= 'A' && item <= 'Z') || (item >= 'a' && item <= 'z') ||
        (item >= '0' && item <= '9') || item == '-' || item == '_' ||
        item == '.' || item == '~';
    if (unreserved) {
      if (!writer.append(static_cast<char>(item))) return false;
    } else if (!writer.append('%') ||
               !writer.append(digits[(item >> 4U) & 0x0fU]) ||
               !writer.append(digits[item & 0x0fU])) {
      return false;
    }
  }
  return true;
}

bool ParseTradeEvent(std::string_view order_id, std::string_view trade_id,
                     std::string_view price, std::string_view size,
                     std::string_view created,
                     api::VenueEvent& event) noexcept {
  event = {};
  event.type = api::VenueEventType::Fill;
  return CopyId(order_id, event.venue_order_id) &&
         CopyId(trade_id, event.trade_id) &&
         ParseFixed(price, event.fill_price) && event.fill_price.value > 0 &&
         ParseFixed(size, event.fill_quantity) &&
         event.fill_quantity.value > 0 &&
         ParseTimeNs(created, event.event_time_ns);
}

}  // namespace

ProtocolResult ScaleToSix(api::FixedPoint value,
                          std::uint64_t& output) noexcept {
  output = 0;
  if (value.value <= 0 || value.scale > 18) return ProtocolResult::InvalidArgument;
  const auto unsigned_value = static_cast<std::uint64_t>(value.value);
  if (value.scale <= 6) {
    const std::uint64_t multiplier =
        Power10(static_cast<std::uint8_t>(6 - value.scale));
    if (unsigned_value > std::numeric_limits<std::uint64_t>::max() / multiplier)
      return ProtocolResult::BoundsExceeded;
    output = unsigned_value * multiplier;
  } else {
    const std::uint64_t divisor =
        Power10(static_cast<std::uint8_t>(value.scale - 6));
    if (unsigned_value % divisor != 0) return ProtocolResult::InvalidArgument;
    output = unsigned_value / divisor;
  }
  return output == 0 ? ProtocolResult::InvalidArgument : ProtocolResult::Ok;
}

ProtocolResult OrderAmounts(api::Side side, api::FixedPoint quantity,
                            api::FixedPoint price, std::uint64_t& maker,
                            std::uint64_t& taker) noexcept {
  std::uint64_t shares = 0;
  if (ScaleToSix(quantity, shares) != ProtocolResult::Ok || price.value <= 0 ||
      price.scale > 18)
    return ProtocolResult::InvalidArgument;
  std::uint64_t left = shares;
  std::uint64_t right = static_cast<std::uint64_t>(price.value);
  std::uint64_t divisor = Power10(price.scale);
  std::uint64_t factor = GreatestCommonDivisor(left, divisor);
  left /= factor;
  divisor /= factor;
  factor = GreatestCommonDivisor(right, divisor);
  right /= factor;
  divisor /= factor;
  if (divisor != 1) return ProtocolResult::InvalidArgument;
  if (left > std::numeric_limits<std::uint64_t>::max() / right)
    return ProtocolResult::BoundsExceeded;
  const std::uint64_t dollars = left * right;
  if (dollars == 0) return ProtocolResult::InvalidArgument;
  if (side == api::Side::Buy) {
    maker = dollars;
    taker = shares;
  } else if (side == api::Side::Sell) {
    maker = shares;
    taker = dollars;
  } else {
    return ProtocolResult::InvalidArgument;
  }
  return ProtocolResult::Ok;
}

ProtocolResult BuildPlaceOrder(const PlaceOrderInput& input,
                               WireRequest& request) noexcept {
  request = {};
  if ((input.order.signature_type != 0 && input.order.signature_type != 3) ||
      input.signature.size == 0 || input.owner.empty() ||
      input.order.salt == 0 ||
      input.order.salt >= (std::uint64_t{1} << 53U) ||
      input.order.maker_amount == 0 || input.order.taker_amount == 0 ||
      input.order.side > 1 ||
      (input.time_in_force == api::TimeInForce::GTD &&
       input.expiration_seconds == 0) ||
      (input.time_in_force != api::TimeInForce::GTD &&
       input.expiration_seconds != 0))
    return input.order.signature_type == 1 ? ProtocolResult::Unsupported
                                           : ProtocolResult::InvalidArgument;
  std::string_view order_type;
  switch (input.time_in_force) {
    case api::TimeInForce::GTC:
      order_type = "GTC";
      break;
    case api::TimeInForce::GTD:
      order_type = "GTD";
      break;
    case api::TimeInForce::FOK:
      order_type = "FOK";
      break;
    case api::TimeInForce::FAK:
    case api::TimeInForce::IOC:
      order_type = "FAK";
      break;
    default:
      return ProtocolResult::InvalidArgument;
  }
  if (!SetMethodPath(request, "POST", "/order"))
    return ProtocolResult::BoundsExceeded;
  Writer writer(request.body);
  const bool ok =
      writer.append("{\"order\":{\"salt\":") &&
      writer.decimal(input.order.salt) &&
      writer.append(",\"maker\":\"0x") &&
      writer.hex(input.order.maker.data(), input.order.maker.size()) &&
      writer.append("\",\"signer\":\"0x") &&
      writer.hex(input.order.signer.data(), input.order.signer.size()) &&
      writer.append("\",\"tokenId\":\"") &&
      [&] {
        std::array<char, 79> token{};
        std::size_t token_size = 0;
        return TokenIdToDecimal(input.order.token_id, token.data(), token.size(),
                                token_size) == CryptoResult::Ok &&
               writer.append(std::string_view(token.data(), token_size));
      }() &&
      writer.append("\",\"makerAmount\":\"") &&
      writer.decimal(input.order.maker_amount) &&
      writer.append("\",\"takerAmount\":\"") &&
      writer.decimal(input.order.taker_amount) &&
      writer.append("\",\"side\":\"") &&
      writer.append(input.order.side == 0 ? "BUY" : "SELL") &&
      writer.append("\",\"signatureType\":") &&
      writer.decimal(input.order.signature_type) &&
      writer.append(",\"timestamp\":\"") &&
      writer.decimal(input.order.timestamp_ms) &&
      writer.append("\",\"metadata\":\"0x") &&
      writer.hex(input.order.metadata.data(), input.order.metadata.size()) &&
      writer.append("\",\"builder\":\"0x") &&
      writer.hex(input.order.builder.data(), input.order.builder.size()) &&
      writer.append("\",\"signature\":\"0x") &&
      writer.hex(input.signature.bytes.data(), input.signature.size) &&
      writer.append("\",\"expiration\":\"") &&
      writer.decimal(input.expiration_seconds) &&
      writer.append("\"},\"owner\":") &&
      writer.json_string(input.owner) && writer.append(",\"orderType\":") &&
      writer.json_string(order_type) &&
      writer.append(",\"deferExec\":false,\"postOnly\":") &&
      writer.append(input.post_only ? "true}" : "false}");
  if (!ok || writer.size() > std::numeric_limits<std::uint16_t>::max())
    return ProtocolResult::BoundsExceeded;
  request.body_size = static_cast<std::uint16_t>(writer.size());
  return ProtocolResult::Ok;
}

ProtocolResult BuildCancelOrder(std::string_view venue_order_id,
                                WireRequest& request) noexcept {
  request = {};
  if (venue_order_id.empty() || venue_order_id.size() > 96)
    return ProtocolResult::InvalidArgument;
  if (!SetMethodPath(request, "DELETE", "/order"))
    return ProtocolResult::BoundsExceeded;
  Writer writer(request.body);
  if (!writer.append("{\"orderID\":") || !writer.json_string(venue_order_id) ||
      !writer.append('}'))
    return ProtocolResult::BoundsExceeded;
  request.body_size = static_cast<std::uint16_t>(writer.size());
  return ProtocolResult::Ok;
}

ProtocolResult BuildOpenOrdersPage(const Pagination& pagination,
                                   WireRequest& request,
                                   std::string_view asset_id) noexcept {
  request = {};
  if (pagination.complete || pagination.page_count >= kMaximumPages ||
      pagination.item_count >= kMaximumOpenOrders ||
      pagination.cursor_size > pagination.cursor.size())
    return ProtocolResult::BoundsExceeded;
  std::array<char, 512> path{};
  Writer writer(path);
  const bool filtered = !asset_id.empty();
  if (!writer.append("/data/orders") ||
      (filtered &&
       (!writer.append("?asset_id=") ||
        !AppendQueryEncoded(writer, asset_id))) ||
      (pagination.cursor_size != 0 &&
       (!writer.append(filtered ? "&next_cursor="
                                : "?next_cursor=") ||
        !AppendQueryEncoded(
            writer, std::string_view(pagination.cursor.data(),
                                     pagination.cursor_size)))))
    return ProtocolResult::BoundsExceeded;
  return SetMethodPath(request, "GET",
                       std::string_view(path.data(), writer.size()))
             ? ProtocolResult::Ok
             : ProtocolResult::BoundsExceeded;
}

ProtocolResult BuildPositions(std::string_view funder,
                              WireRequest& request,
                              std::string_view market) noexcept {
  request = {};
  if (funder.empty()) return ProtocolResult::InvalidArgument;
  std::array<char, 512> path{};
  Writer writer(path);
  if (!writer.append("/positions?user=") ||
      !AppendQueryEncoded(writer, funder) ||
      !writer.append("&sizeThreshold=0.0001&limit=500") ||
      (!market.empty() &&
       (!writer.append("&market=") ||
        !AppendQueryEncoded(writer, market))))
    return ProtocolResult::BoundsExceeded;
  return SetMethodPath(request, "GET",
                       std::string_view(path.data(), writer.size()))
             ? ProtocolResult::Ok
             : ProtocolResult::BoundsExceeded;
}

ProtocolResult ParseOpenOrdersPage(std::string_view json,
                                   Pagination& pagination,
                                   api::VenueEvent* events,
                                   std::size_t event_capacity,
                                   std::size_t& event_count) noexcept {
  event_count = 0;
  if (json.empty() || json.size() > kMaximumWireBytes ||
      pagination.complete || pagination.page_count >= kMaximumPages)
    return ProtocolResult::BoundsExceeded;
  std::string_view array;
  if (!ArrayRange(json, array)) return ProtocolResult::Malformed;
  std::size_t offset = 0;
  std::string_view object;
  std::size_t raw_count = 0;
  bool malformed = false;
  while (NextObject(array, offset, object, malformed)) {
    ++raw_count;
    if (pagination.item_count + raw_count > kMaximumOpenOrders)
      return ProtocolResult::BoundsExceeded;
    std::string_view id;
    std::string_view status;
    std::string_view original;
    std::string_view matched;
    std::string_view price;
    if (!JsonString(object, "id", id) || !JsonString(object, "status", status) ||
        !JsonString(object, "original_size", original) ||
        !JsonString(object, "size_matched", matched) ||
        !JsonString(object, "price", price))
      return ProtocolResult::Malformed;
    api::FixedPoint original_value{};
    api::FixedPoint matched_value{};
    if (!ParseFixed(original, original_value) ||
        !ParseFixed(matched, matched_value) || original_value.value <= 0 ||
        matched_value.value < 0)
      return ProtocolResult::Malformed;
    const bool live = EqualFold(status, "LIVE") ||
                      EqualFold(status, "ORDER_STATUS_LIVE");
    if (!live) continue;
    if (CompareFixed(matched_value, original_value) >= 0) continue;
    if (event_count == event_capacity || events == nullptr)
      return ProtocolResult::BoundsExceeded;
    api::VenueEvent& event = events[event_count++];
    event = {};
    event.type = api::VenueEventType::ReconcileOpen;
    event.reconciled_status =
        matched_value.value == 0 ? api::OrderStatus::Open
                                 : api::OrderStatus::PartiallyFilled;
    if (!CopyId(id, event.venue_order_id) ||
        !ParseFixed(price, event.fill_price) || event.fill_price.value <= 0)
      return ProtocolResult::Malformed;
  }
  if (malformed) return ProtocolResult::Malformed;
  ++pagination.page_count;
  pagination.item_count = static_cast<std::uint16_t>(
      pagination.item_count + static_cast<std::uint16_t>(raw_count));
  std::string_view next;
  const bool has_cursor = JsonString(json, "next_cursor", next);
  if (Trim(json).starts_with('[') || !has_cursor || next.empty() ||
      next == "LTE=" || pagination.page_count >= kMaximumPages ||
      pagination.item_count >= kMaximumOpenOrders) {
    pagination.complete = true;
    pagination.cursor_size = 0;
    return ProtocolResult::Ok;
  }
  if (next.size() > kMaximumCursorBytes) return ProtocolResult::BoundsExceeded;
  for (std::size_t index = 0; index < pagination.page_count; ++index) {
    if (pagination.seen_sizes[index] == next.size() &&
        std::equal(next.begin(), next.end(), pagination.seen[index].begin())) {
      pagination.complete = true;
      pagination.cursor_size = 0;
      return ProtocolResult::Ok;
    }
  }
  const std::size_t seen_index = pagination.page_count - 1;
  std::copy(next.begin(), next.end(), pagination.seen[seen_index].begin());
  pagination.seen_sizes[seen_index] =
      static_cast<std::uint16_t>(next.size());
  std::copy(next.begin(), next.end(), pagination.cursor.begin());
  pagination.cursor_size = static_cast<std::uint16_t>(next.size());
  return ProtocolResult::Ok;
}

ProtocolResult ParseOpenOrderSnapshots(
    std::string_view json, Pagination& pagination, OpenOrderSnapshot* items,
    std::size_t item_capacity, std::size_t& item_count) noexcept {
  item_count = 0;
  if (json.empty() || json.size() > kMaximumWireBytes ||
      pagination.complete || pagination.page_count >= kMaximumPages)
    return ProtocolResult::BoundsExceeded;
  std::string_view array;
  if (!ArrayRange(json, array)) return ProtocolResult::Malformed;
  std::size_t offset = 0;
  std::string_view object;
  std::size_t raw_count = 0;
  bool malformed = false;
  while (NextObject(array, offset, object, malformed)) {
    ++raw_count;
    if (pagination.item_count + raw_count > kMaximumOpenOrders)
      return ProtocolResult::BoundsExceeded;
    std::string_view id, asset, side, status, original, matched, price;
    if (!JsonString(object, "id", id) ||
        !JsonString(object, "asset_id", asset) ||
        !JsonString(object, "side", side) ||
        !JsonString(object, "status", status) ||
        !JsonString(object, "original_size", original) ||
        !JsonString(object, "size_matched", matched) ||
        !JsonString(object, "price", price))
      return ProtocolResult::Malformed;
    api::FixedPoint original_value{}, matched_value{};
    const bool live = EqualFold(status, "LIVE") ||
                      EqualFold(status, "ORDER_STATUS_LIVE");
    if (!ParseFixed(original, original_value) ||
        !ParseFixed(matched, matched_value) || original_value.value <= 0 ||
        matched_value.value < 0)
      return ProtocolResult::Malformed;
    if (!live || CompareFixed(matched_value, original_value) >= 0) continue;
    if (items == nullptr || item_count == item_capacity)
      return ProtocolResult::BoundsExceeded;
    OpenOrderSnapshot& item = items[item_count++];
    item = {};
    item.status = matched_value.value == 0 ? api::OrderStatus::Open
                                           : api::OrderStatus::PartiallyFilled;
    item.side = EqualFold(side, "BUY") ? api::Side::Buy : api::Side::Sell;
    if ((!EqualFold(side, "BUY") && !EqualFold(side, "SELL")) ||
        !CopyId(id, item.venue_order_id) ||
        TokenIdFromDecimal(asset, item.token_id) != CryptoResult::Ok ||
        !ParseFixed(price, item.price) || item.price.value <= 0)
      return ProtocolResult::Malformed;
    item.quantity = original_value;
    item.matched_quantity = matched_value;
  }
  if (malformed) return ProtocolResult::Malformed;
  ++pagination.page_count;
  pagination.item_count = static_cast<std::uint16_t>(
      pagination.item_count + static_cast<std::uint16_t>(raw_count));
  std::string_view next;
  const bool has_cursor = JsonString(json, "next_cursor", next);
  if (Trim(json).starts_with('[') || !has_cursor || next.empty() ||
      next == "LTE=" || pagination.page_count >= kMaximumPages ||
      pagination.item_count >= kMaximumOpenOrders) {
    pagination.complete = true;
    pagination.cursor_size = 0;
    return ProtocolResult::Ok;
  }
  if (next.size() > kMaximumCursorBytes) return ProtocolResult::BoundsExceeded;
  for (std::size_t index = 0; index < pagination.page_count; ++index) {
    if (pagination.seen_sizes[index] == next.size() &&
        std::equal(next.begin(), next.end(), pagination.seen[index].begin())) {
      pagination.complete = true;
      pagination.cursor_size = 0;
      return ProtocolResult::Ok;
    }
  }
  const std::size_t seen_index = pagination.page_count - 1;
  std::copy(next.begin(), next.end(), pagination.seen[seen_index].begin());
  pagination.seen_sizes[seen_index] =
      static_cast<std::uint16_t>(next.size());
  std::copy(next.begin(), next.end(), pagination.cursor.begin());
  pagination.cursor_size = static_cast<std::uint16_t>(next.size());
  return ProtocolResult::Ok;
}

ProtocolResult ParsePositions(std::string_view json, PositionSnapshot* items,
                              std::size_t item_capacity,
                              std::size_t& item_count) noexcept {
  item_count = 0;
  if (json.empty() || json.size() > kMaximumWireBytes)
    return ProtocolResult::BoundsExceeded;
  std::string_view array;
  if (!ArrayRange(json, array)) return ProtocolResult::Malformed;
  std::size_t offset = 0;
  std::string_view object;
  bool malformed = false;
  while (NextObject(array, offset, object, malformed)) {
    if (items == nullptr || item_count == item_capacity)
      return ProtocolResult::BoundsExceeded;
    std::string_view asset, size;
    if (!JsonString(object, "asset", asset) ||
        !JsonScalar(object, "size", size))
      return ProtocolResult::Malformed;
    PositionSnapshot& item = items[item_count++];
    item = {};
    if (TokenIdFromDecimal(asset, item.token_id) != CryptoResult::Ok ||
        !ParseFixed(size, item.quantity) || item.quantity.value <= 0)
      return ProtocolResult::Malformed;
  }
  return malformed ? ProtocolResult::Malformed : ProtocolResult::Ok;
}

ProtocolResult ParsePlaceResponse(std::string_view json,
                                  const api::OrderHandle& handle,
                                  const api::RequestToken& token,
                                  api::VenueEvent& event) noexcept {
  if (json.empty() || json.size() > kMaximumWireBytes)
    return ProtocolResult::BoundsExceeded;
  bool success = false;
  std::string_view id;
  std::string_view status;
  if (!JsonBool(json, "success", success)) return ProtocolResult::Malformed;
  event = {};
  event.handle = handle;
  event.token = token;
  if (!success) {
    event.type = api::VenueEventType::NewReject;
    return ProtocolResult::Ok;
  }
  if (!JsonString(json, "orderID", id) || id.empty() ||
      !JsonString(json, "status", status) ||
      !CopyId(id, event.venue_order_id))
    return ProtocolResult::Malformed;
  event.type = api::VenueEventType::NewAck;
  return ProtocolResult::Ok;
}

ProtocolResult ParseCancelResponse(std::string_view json,
                                   std::string_view expected_order_id,
                                   const api::OrderHandle& handle,
                                   const api::RequestToken& token,
                                   api::VenueEvent& event) noexcept {
  if (json.empty() || json.size() > kMaximumWireBytes ||
      expected_order_id.empty())
    return ProtocolResult::InvalidArgument;
  event = {};
  event.handle = handle;
  event.token = token;
  event.type = api::VenueEventType::CancelReject;
  if (!CopyId(expected_order_id, event.venue_order_id))
    return ProtocolResult::InvalidArgument;
  const std::size_t canceled = json.find("\"canceled\"");
  if (canceled == std::string_view::npos) return ProtocolResult::Malformed;
  const std::size_t begin = json.find('[', canceled);
  const std::size_t end =
      begin == std::string_view::npos ? begin : json.find(']', begin);
  if (begin == std::string_view::npos || end == std::string_view::npos)
    return ProtocolResult::Malformed;
  const std::string_view list = json.substr(begin, end - begin + 1);
  std::size_t offset = 0;
  while (offset < list.size()) {
    const std::size_t quote = list.find('"', offset);
    if (quote == std::string_view::npos) break;
    const std::size_t close = list.find('"', quote + 1);
    if (close == std::string_view::npos) return ProtocolResult::Malformed;
    if (list.substr(quote + 1, close - quote - 1) == expected_order_id) {
      event.type = api::VenueEventType::CancelAck;
      break;
    }
    offset = close + 1;
  }
  return ProtocolResult::Ok;
}

ProtocolResult ParseUserMessage(std::string_view json,
                                api::VenueEvent& event) noexcept {
  std::size_t count = 0;
  const ProtocolResult result = ParseUserMessageEvents(json, &event, 1, count);
  return result == ProtocolResult::Ok && count != 1
             ? ProtocolResult::BoundsExceeded
             : result;
}

ProtocolResult ParseUserMessageEvents(std::string_view json,
                                      api::VenueEvent* events,
                                      std::size_t event_capacity,
                                      std::size_t& event_count) noexcept {
  event_count = 0;
  if (json.empty() || json.size() > kMaximumWireBytes)
    return ProtocolResult::BoundsExceeded;
  if (events == nullptr || event_capacity == 0 ||
      event_capacity > kMaximumTradeEvents)
    return ProtocolResult::InvalidArgument;
  const std::string_view trimmed = Trim(json);
  if (EqualFold(trimmed, "PING") || EqualFold(trimmed, "PONG"))
    return ProtocolResult::Unsupported;
  std::string_view event_type;
  std::string_view id;
  if (!JsonString(json, "event_type", event_type) ||
      !JsonString(json, "id", id))
    return ProtocolResult::Malformed;
  if (EqualFold(event_type, "trade")) {
    std::string_view price;
    std::string_view size;
    std::string_view created;
    if (!JsonString(json, "price", price) ||
        (!JsonString(json, "size_matched", size) &&
         !JsonString(json, "size", size)) ||
        !JsonString(json, "created_at", created))
      return ProtocolResult::Malformed;
    std::string_view maker_orders;
    const bool has_makers = ArrayFieldRange(json, "maker_orders", maker_orders);
    const std::size_t maker_field = json.find("\"maker_orders\"");
    if (maker_field != std::string_view::npos && !has_makers)
      return ProtocolResult::Malformed;
    const std::string_view top_level =
        maker_field == std::string_view::npos ? json : json.substr(0, maker_field);
    std::string_view order_id;
    if (!JsonString(top_level, "order_id", order_id))
      (void)JsonString(top_level, "taker_order_id", order_id);
    if (!order_id.empty()) {
      if (event_count == event_capacity)
        return ProtocolResult::BoundsExceeded;
      if (!ParseTradeEvent(order_id, id, price, size, created,
                           events[event_count]))
        return ProtocolResult::Malformed;
      ++event_count;
    }
    if (has_makers) {
      std::size_t offset = 0;
      std::string_view maker;
      bool malformed = false;
      while (NextObject(maker_orders, offset, maker, malformed)) {
        if (event_count == event_capacity ||
            event_count == kMaximumTradeEvents)
          return ProtocolResult::BoundsExceeded;
        std::string_view maker_order_id;
        std::string_view maker_price;
        std::string_view maker_size;
        if (!JsonString(maker, "order_id", maker_order_id) ||
            !JsonString(maker, "price", maker_price) ||
            (!JsonString(maker, "matched_amount", maker_size) &&
             !JsonString(maker, "size_matched", maker_size)) ||
            !ParseTradeEvent(maker_order_id, id, maker_price, maker_size,
                             created, events[event_count]))
          return ProtocolResult::Malformed;
        ++event_count;
      }
      if (malformed) return ProtocolResult::Malformed;
    }
    if (event_count == 0) return ProtocolResult::Malformed;
    return ProtocolResult::Ok;
  }
  api::VenueEvent& event = events[0];
  event = {};
  if (!EqualFold(event_type, "order") ||
      !CopyId(id, event.venue_order_id))
    return ProtocolResult::Unsupported;
  std::string_view status;
  std::string_view update_type;
  if (!JsonString(json, "status", status) ||
      !JsonString(json, "type", update_type))
    return ProtocolResult::Malformed;
  if (EqualFold(update_type, "CANCELLATION") ||
      EqualFold(status, "CANCELED")) {
    event.type = api::VenueEventType::CancelAck;
  } else if (EqualFold(status, "LIVE") ||
             EqualFold(status, "ORDER_STATUS_LIVE")) {
    event.type = api::VenueEventType::NewAck;
  } else if (EqualFold(status, "MATCHED") ||
             EqualFold(status, "ORDER_STATUS_MATCHED")) {
    event.type = api::VenueEventType::ReconcileTerminal;
    event.reconciled_status = api::OrderStatus::Filled;
  } else {
    return ProtocolResult::Unsupported;
  }
  std::string_view created;
  if (JsonString(json, "created_at", created) &&
      !ParseTimeNs(created, event.event_time_ns))
    return ProtocolResult::Malformed;
  event_count = 1;
  return ProtocolResult::Ok;
}

}  // namespace oms::exchange::polymarket

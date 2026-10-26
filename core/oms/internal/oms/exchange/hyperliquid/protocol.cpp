#include "oms/exchange/hyperliquid/protocol.h"

#include <atomic>
#include <charconv>
#include <chrono>
#include <cstring>
#include <limits>
#include <utility>

namespace oms::exchange::hyperliquid {
namespace {

constexpr std::size_t kMaximumJsonBytes = 4U << 20U;

class Packer {
 public:
  void map(std::uint8_t count) { bytes_.push_back(0x80U | count); }
  void array(std::uint8_t count) { bytes_.push_back(0x90U | count); }

  void str(std::string_view value) {
    if (value.size() < 32) {
      bytes_.push_back(static_cast<std::uint8_t>(0xa0U | value.size()));
    } else {
      bytes_.push_back(0xd9U);
      bytes_.push_back(static_cast<std::uint8_t>(value.size()));
    }
    bytes_.insert(bytes_.end(), value.begin(), value.end());
  }

  void boolean(bool value) { bytes_.push_back(value ? 0xc3U : 0xc2U); }

  void integer(std::int64_t value) {
    if (value >= 0 && value < 128) {
      bytes_.push_back(static_cast<std::uint8_t>(value));
      return;
    }
    const auto positive = static_cast<std::uint64_t>(value);
    bytes_.push_back(0xceU);
    for (int shift = 24; shift >= 0; shift -= 8) {
      bytes_.push_back(static_cast<std::uint8_t>((positive >> shift) & 0xffU));
    }
  }

  [[nodiscard]] std::vector<std::uint8_t> release() { return std::move(bytes_); }

 private:
  std::vector<std::uint8_t> bytes_;
};

void store_be(std::uint8_t* out, std::uint64_t value) noexcept {
  std::memset(out, 0, 32);
  for (int index = 0; index < 8; ++index) {
    out[31 - index] = static_cast<std::uint8_t>((value >> (8 * index)) & 0xffU);
  }
}

polymarket::Bytes32 hash_text(std::string_view text) noexcept {
  return polymarket::Keccak256(
      reinterpret_cast<const std::uint8_t*>(text.data()), text.size());
}

enum class JsonType : std::uint8_t {
  Null,
  Boolean,
  Number,
  String,
  Array,
  Object,
};

struct JsonValue {
  JsonType type{JsonType::Null};
  std::string_view scalar{};
  std::vector<JsonValue> array;
  std::vector<std::pair<std::string_view, JsonValue>> object;

  [[nodiscard]] const JsonValue* find(std::string_view name) const noexcept {
    if (type != JsonType::Object) return nullptr;
    for (const auto& field : object)
      if (field.first == name) return &field.second;
    return nullptr;
  }
};

class JsonParser {
 public:
  explicit JsonParser(std::string_view input) : input_(input) {}

  bool parse(JsonValue& output) {
    space();
    if (!value(output)) return false;
    space();
    return position_ == input_.size();
  }

 private:
  void space() noexcept {
    while (position_ < input_.size() &&
           (input_[position_] == ' ' || input_[position_] == '\n' ||
            input_[position_] == '\r' || input_[position_] == '\t'))
      ++position_;
  }

  bool take(char expected) noexcept {
    if (position_ >= input_.size() || input_[position_] != expected)
      return false;
    ++position_;
    return true;
  }

  bool literal(std::string_view text) noexcept {
    if (input_.substr(position_, text.size()) != text) return false;
    position_ += text.size();
    return true;
  }

  bool string(std::string_view& output) noexcept {
    if (!take('"')) return false;
    const std::size_t begin = position_;
    while (position_ < input_.size()) {
      const char current = input_[position_++];
      if (current == '"') {
        output = input_.substr(begin, position_ - begin - 1);
        return true;
      }
      // Hyperliquid identifiers and decimal fields do not require JSON
      // unescaping. Reject escapes so callers never compare encoded text.
      if (current == '\\' || static_cast<unsigned char>(current) < 0x20U)
        return false;
    }
    return false;
  }

  bool number(std::string_view& output) noexcept {
    const std::size_t begin = position_;
    if (position_ < input_.size() && input_[position_] == '-') ++position_;
    const std::size_t integer = position_;
    while (position_ < input_.size() && input_[position_] >= '0' &&
           input_[position_] <= '9')
      ++position_;
    if (position_ == integer) return false;
    if (position_ < input_.size() && input_[position_] == '.') {
      ++position_;
      const std::size_t fraction = position_;
      while (position_ < input_.size() && input_[position_] >= '0' &&
             input_[position_] <= '9')
        ++position_;
      if (position_ == fraction) return false;
    }
    if (position_ < input_.size() &&
        (input_[position_] == 'e' || input_[position_] == 'E')) {
      ++position_;
      if (position_ < input_.size() &&
          (input_[position_] == '+' || input_[position_] == '-'))
        ++position_;
      const std::size_t exponent = position_;
      while (position_ < input_.size() && input_[position_] >= '0' &&
             input_[position_] <= '9')
        ++position_;
      if (position_ == exponent) return false;
    }
    output = input_.substr(begin, position_ - begin);
    return true;
  }

  bool value(JsonValue& output) {
    space();
    if (position_ >= input_.size()) return false;
    if (input_[position_] == '"') {
      output.type = JsonType::String;
      return string(output.scalar);
    }
    if (input_[position_] == '{') {
      output.type = JsonType::Object;
      ++position_;
      space();
      if (take('}')) return true;
      for (;;) {
        std::string_view name;
        if (!string(name)) return false;
        for (const auto& field : output.object)
          if (field.first == name) return false;
        space();
        if (!take(':')) return false;
        JsonValue child;
        if (!value(child)) return false;
        output.object.emplace_back(name, std::move(child));
        space();
        if (take('}')) return true;
        if (!take(',')) return false;
        space();
      }
    }
    if (input_[position_] == '[') {
      output.type = JsonType::Array;
      ++position_;
      space();
      if (take(']')) return true;
      for (;;) {
        JsonValue child;
        if (!value(child)) return false;
        output.array.push_back(std::move(child));
        space();
        if (take(']')) return true;
        if (!take(',')) return false;
        space();
      }
    }
    if (input_[position_] == 'n' && literal("null")) {
      output.type = JsonType::Null;
      return true;
    }
    if (input_[position_] == 't' && literal("true")) {
      output.type = JsonType::Boolean;
      output.scalar = "true";
      return true;
    }
    if (input_[position_] == 'f' && literal("false")) {
      output.type = JsonType::Boolean;
      output.scalar = "false";
      return true;
    }
    output.type = JsonType::Number;
    return number(output.scalar);
  }

  std::string_view input_;
  std::size_t position_{};
};

bool text(const JsonValue* value, std::string_view& output) noexcept {
  if (value == nullptr ||
      (value->type != JsonType::String && value->type != JsonType::Number))
    return false;
  output = value->scalar;
  return true;
}

bool unsigned_number(const JsonValue* value, std::uint64_t& output) noexcept {
  std::string_view source;
  if (!text(value, source) || source.empty() || source.front() == '-')
    return false;
  const auto result =
      std::from_chars(source.data(), source.data() + source.size(), output);
  return result.ec == std::errc{} &&
         result.ptr == source.data() + source.size();
}

bool fixed_point(const JsonValue* value, api::FixedPoint& output) noexcept {
  std::string_view source;
  if (!text(value, source) || source.empty()) return false;
  bool negative = false;
  std::size_t cursor = 0;
  if (source[cursor] == '-') {
    negative = true;
    if (++cursor == source.size()) return false;
  }
  std::uint64_t magnitude = 0;
  std::uint8_t scale = 0;
  bool dot = false;
  bool digit = false;
  for (; cursor < source.size(); ++cursor) {
    const char character = source[cursor];
    if (character == '.') {
      if (dot || !digit) return false;
      dot = true;
      continue;
    }
    if (character < '0' || character > '9') return false;
    digit = true;
    if (dot) {
      if (scale == 18) return false;
      ++scale;
    }
    const std::uint64_t next =
        static_cast<std::uint64_t>(character - '0');
    if (magnitude >
        (static_cast<std::uint64_t>(std::numeric_limits<std::int64_t>::max()) -
         next) /
            10U)
      return false;
    magnitude = magnitude * 10U + next;
  }
  if (!digit || source.back() == '.') return false;
  output.value = negative ? -static_cast<std::int64_t>(magnitude)
                          : static_cast<std::int64_t>(magnitude);
  output.scale = scale;
  return true;
}

bool copy_text(const JsonValue* value, std::string& output) {
  std::string_view source;
  if (!text(value, source)) return false;
  output.assign(source);
  return true;
}

const JsonValue* metadata_object(const JsonValue& root) noexcept {
  if (root.type == JsonType::Object) return &root;
  if (root.type == JsonType::Array && !root.array.empty() &&
      root.array.front().type == JsonType::Object)
    return &root.array.front();
  return nullptr;
}

const JsonValue* event_array(const JsonValue& root,
                             std::string_view channel) noexcept {
  if (root.type == JsonType::Array) return &root;
  if (root.type != JsonType::Object) return nullptr;
  const JsonValue* data = root.find("data");
  if (data == nullptr) return nullptr;
  if (channel == "userFills" && data->type == JsonType::Object) {
    const JsonValue* fills = data->find("fills");
    return fills == nullptr ? data : fills;
  }
  return data;
}

bool parse_fill(const JsonValue& value, UserEvent& output) {
  if (value.type != JsonType::Object) return false;
  output = {};
  output.kind = UserEventKind::Fill;
  if (!copy_text(value.find("coin"), output.coin) ||
      !copy_text(value.find("tid"), output.tid) ||
      !fixed_point(value.find("sz"), output.size) ||
      !fixed_point(value.find("px"), output.price) ||
      !unsigned_number(value.find("time"), output.time_ms))
    return false;
  (void)copy_text(value.find("cloid"), output.cloid);
  (void)copy_text(value.find("oid"), output.oid);
  return true;
}

bool parse_order(const JsonValue& value, UserEvent& output) {
  if (value.type != JsonType::Object) return false;
  const JsonValue* order = value.find("order");
  if (order == nullptr) order = &value;
  if (order->type != JsonType::Object) return false;
  output = {};
  output.kind = UserEventKind::Order;
  if (!copy_text(order->find("coin"), output.coin) ||
      !copy_text(order->find("oid"), output.oid) ||
      !fixed_point(order->find("sz"), output.size))
    return false;
  (void)copy_text(order->find("cloid"), output.cloid);
  if (!copy_text(value.find("status"), output.status))
    (void)copy_text(order->find("status"), output.status);
  const JsonValue* timestamp = value.find("statusTimestamp");
  if (timestamp == nullptr) timestamp = order->find("timestamp");
  if (timestamp != nullptr &&
      !unsigned_number(timestamp, output.time_ms))
    return false;
  return !output.status.empty();
}

const JsonValue* first_status(const JsonValue& root) noexcept {
  const JsonValue* response = root.find("response");
  if (response == nullptr || response->type != JsonType::Object) return nullptr;
  const JsonValue* data = response->find("data");
  if (data == nullptr || data->type != JsonType::Object) return nullptr;
  const JsonValue* statuses = data->find("statuses");
  if (statuses == nullptr || statuses->type != JsonType::Array ||
      statuses->array.empty())
    return nullptr;
  return &statuses->array.front();
}

}  // namespace

std::vector<std::uint8_t> encode_order_action(const LimitOrder& order) {
  Packer packer;
  packer.map(3);
  packer.str("type");
  packer.str("order");
  packer.str("orders");
  packer.array(1);
  const bool with_cloid = !order.cloid.empty();
  packer.map(with_cloid ? 7 : 6);
  packer.str("a");
  packer.integer(order.asset);
  packer.str("b");
  packer.boolean(order.buy);
  packer.str("p");
  packer.str(order.price);
  packer.str("s");
  packer.str(order.size);
  packer.str("r");
  packer.boolean(order.reduce_only);
  packer.str("t");
  packer.map(1);
  packer.str("limit");
  packer.map(1);
  packer.str("tif");
  packer.str(order.tif);
  if (with_cloid) {
    packer.str("c");
    packer.str(order.cloid);
  }
  packer.str("grouping");
  packer.str("na");
  return packer.release();
}

std::vector<std::uint8_t> encode_cancel_by_cloid(std::int64_t asset,
                                                  std::string_view cloid) {
  Packer packer;
  packer.map(2);
  packer.str("type");
  packer.str("cancelByCloid");
  packer.str("cancels");
  packer.array(1);
  packer.map(2);
  packer.str("asset");
  packer.integer(asset);
  packer.str("cloid");
  packer.str(cloid);
  return packer.release();
}

polymarket::Bytes32 action_hash(const std::uint8_t* action, std::size_t size,
                                std::uint64_t nonce,
                                const polymarket::Address* vault) noexcept {
  std::vector<std::uint8_t> data(action, action + size);
  for (int shift = 56; shift >= 0; shift -= 8) {
    data.push_back(static_cast<std::uint8_t>((nonce >> shift) & 0xffU));
  }
  if (vault == nullptr) {
    data.push_back(0x00);
  } else {
    data.push_back(0x01);
    data.insert(data.end(), vault->begin(), vault->end());
  }
  return polymarket::Keccak256(data.data(), data.size());
}

polymarket::Bytes32 agent_digest(const polymarket::Bytes32& connection_id,
                                 bool mainnet) noexcept {
  const auto domain_type = hash_text(
      "EIP712Domain(string name,string version,uint256 chainId,address "
      "verifyingContract)");
  const auto name = hash_text("Exchange");
  const auto version = hash_text("1");
  std::array<std::uint8_t, 160> domain{};
  std::memcpy(domain.data(), domain_type.data(), 32);
  std::memcpy(domain.data() + 32, name.data(), 32);
  std::memcpy(domain.data() + 64, version.data(), 32);
  store_be(domain.data() + 96, 1337);
  const auto domain_separator = polymarket::Keccak256(domain.data(), domain.size());

  const auto agent_type = hash_text("Agent(string source,bytes32 connectionId)");
  const auto source = hash_text(mainnet ? "a" : "b");
  std::array<std::uint8_t, 96> agent{};
  std::memcpy(agent.data(), agent_type.data(), 32);
  std::memcpy(agent.data() + 32, source.data(), 32);
  std::memcpy(agent.data() + 64, connection_id.data(), 32);
  const auto agent_hash = polymarket::Keccak256(agent.data(), agent.size());

  std::array<std::uint8_t, 66> digest{};
  digest[0] = 0x19;
  digest[1] = 0x01;
  std::memcpy(digest.data() + 2, domain_separator.data(), 32);
  std::memcpy(digest.data() + 34, agent_hash.data(), 32);
  return polymarket::Keccak256(digest.data(), digest.size());
}

std::uint32_t stable_strategy_hash(
    std::string_view strategy_instance) noexcept {
  std::uint32_t hash = 2166136261U;
  for (const char raw_byte : strategy_instance) {
    const auto byte = static_cast<unsigned char>(raw_byte);
    hash ^= byte;
    hash *= 16777619U;
  }
  return hash;
}

std::uint32_t make_process_epoch() noexcept {
  static std::atomic<std::uint32_t> counter{0};
  const auto ticks = static_cast<std::uint64_t>(
      std::chrono::system_clock::now().time_since_epoch().count());
  const auto address =
      static_cast<std::uint64_t>(reinterpret_cast<std::uintptr_t>(&counter));
  const std::uint64_t mixed =
      ticks ^ (ticks >> 23U) ^ address ^ (address >> 17U) ^
      static_cast<std::uint64_t>(counter.fetch_add(1, std::memory_order_relaxed));
  return static_cast<std::uint32_t>(mixed & 0x00ffffffU);
}

bool encode_cloid(const CloidFields& fields, std::string& output) noexcept {
  if (fields.strategy_instance_hash == 0 ||
      fields.process_epoch > 0x00ffffffU ||
      fields.sequence == 0 || fields.sequence > kMaximumCloidSequence)
    return false;
  std::array<std::uint8_t, 16> bytes{};
  bytes[0] = static_cast<std::uint8_t>(kCloidMagic >> 8U);
  bytes[1] = static_cast<std::uint8_t>(kCloidMagic);
  bytes[2] = kCloidVersion;
  for (std::size_t index = 0; index < 4; ++index)
    bytes[3 + index] = static_cast<std::uint8_t>(
        fields.strategy_instance_hash >> (24U - 8U * index));
  for (std::size_t index = 0; index < 3; ++index)
    bytes[7 + index] = static_cast<std::uint8_t>(
        fields.process_epoch >> (16U - 8U * index));
  for (std::size_t index = 0; index < 6; ++index)
    bytes[10 + index] = static_cast<std::uint8_t>(
        fields.sequence >> (40U - 8U * index));
  static constexpr char kHex[] = "0123456789abcdef";
  try {
    output.assign(34, '0');
  } catch (...) {
    return false;
  }
  output[1] = 'x';
  for (std::size_t index = 0; index < bytes.size(); ++index) {
    output[2 + index * 2] = kHex[bytes[index] >> 4U];
    output[3 + index * 2] = kHex[bytes[index] & 0x0fU];
  }
  return true;
}

bool decode_cloid(std::string_view text, CloidFields& output) noexcept {
  if (text.size() != 34 || text[0] != '0' ||
      (text[1] != 'x' && text[1] != 'X'))
    return false;
  std::array<std::uint8_t, 16> bytes{};
  for (std::size_t index = 0; index < bytes.size(); ++index) {
    const auto nibble = [](char value, std::uint8_t& result) noexcept {
      if (value >= '0' && value <= '9')
        result = static_cast<std::uint8_t>(value - '0');
      else if (value >= 'a' && value <= 'f')
        result = static_cast<std::uint8_t>(value - 'a' + 10);
      else if (value >= 'A' && value <= 'F')
        result = static_cast<std::uint8_t>(value - 'A' + 10);
      else
        return false;
      return true;
    };
    std::uint8_t high{};
    std::uint8_t low{};
    if (!nibble(text[2 + index * 2], high) ||
        !nibble(text[3 + index * 2], low))
      return false;
    bytes[index] = static_cast<std::uint8_t>((high << 4U) | low);
  }
  if ((static_cast<std::uint16_t>(bytes[0]) << 8U | bytes[1]) !=
          kCloidMagic ||
      bytes[2] != kCloidVersion)
    return false;
  CloidFields decoded{};
  for (std::size_t index = 0; index < 4; ++index)
    decoded.strategy_instance_hash =
        (decoded.strategy_instance_hash << 8U) | bytes[3 + index];
  for (std::size_t index = 0; index < 3; ++index)
    decoded.process_epoch = (decoded.process_epoch << 8U) | bytes[7 + index];
  for (std::size_t index = 0; index < 6; ++index)
    decoded.sequence = (decoded.sequence << 8U) | bytes[10 + index];
  if (decoded.strategy_instance_hash == 0 || decoded.sequence == 0)
    return false;
  output = decoded;
  return true;
}

bool cloid_owned_by(std::string_view text,
                    std::uint32_t strategy_instance_hash) noexcept {
  CloidFields decoded{};
  return decode_cloid(text, decoded) &&
         decoded.strategy_instance_hash == strategy_instance_hash;
}

JsonParseResult parse_perp_dexs(
    std::string_view json, std::vector<PerpDex>& output) noexcept {
  if (json.size() > kMaximumJsonBytes) return JsonParseResult::Oversize;
  try {
    JsonValue root;
    if (!JsonParser(json).parse(root) || root.type != JsonType::Array)
      return JsonParseResult::Malformed;
    std::vector<PerpDex> parsed;
    parsed.reserve(root.array.size());
    for (std::size_t index = 0; index < root.array.size(); ++index) {
      const JsonValue& item = root.array[index];
      PerpDex dex;
      dex.index = static_cast<std::int64_t>(index);
      if (item.type == JsonType::Null) {
        if (index != 0) return JsonParseResult::InvalidValue;
      } else if (item.type == JsonType::Object) {
        if (!copy_text(item.find("name"), dex.name) || dex.name.empty())
          return JsonParseResult::MissingField;
      } else {
        return JsonParseResult::InvalidValue;
      }
      parsed.push_back(std::move(dex));
    }
    if (parsed.empty() || !parsed.front().name.empty())
      return JsonParseResult::InvalidValue;
    output = std::move(parsed);
    return JsonParseResult::Ok;
  } catch (...) {
    return JsonParseResult::Oversize;
  }
}

JsonParseResult parse_perp_meta(
    std::string_view json, std::int64_t dex_index, std::string_view dex_name,
    std::vector<PerpAsset>& output) noexcept {
  if (json.size() > kMaximumJsonBytes) return JsonParseResult::Oversize;
  try {
    JsonValue root;
    if (!JsonParser(json).parse(root)) return JsonParseResult::Malformed;
    const JsonValue* metadata = metadata_object(root);
    if (metadata == nullptr) return JsonParseResult::InvalidValue;
    const JsonValue* universe = metadata->find("universe");
    if (universe == nullptr || universe->type != JsonType::Array)
      return JsonParseResult::MissingField;
    std::vector<PerpAsset> parsed;
    parsed.reserve(universe->array.size());
    for (std::size_t index = 0; index < universe->array.size(); ++index) {
      const JsonValue& item = universe->array[index];
      if (item.type != JsonType::Object)
        return JsonParseResult::InvalidValue;
      PerpAsset asset;
      asset.dex_index = dex_index;
      asset.asset_index = static_cast<std::int64_t>(index);
      std::string_view name;
      std::uint64_t decimals{};
      if (!text(item.find("name"), name) || name.empty() ||
          !unsigned_number(item.find("szDecimals"), decimals) ||
          decimals > 18)
        return JsonParseResult::InvalidValue;
      if (dex_index != 0 && name.find(':') == std::string_view::npos) {
        asset.symbol.assign(dex_name);
        asset.symbol.push_back(':');
        asset.symbol.append(name);
      } else {
        asset.symbol.assign(name);
      }
      asset.size_decimals = static_cast<std::uint8_t>(decimals);
      parsed.push_back(std::move(asset));
    }
    output = std::move(parsed);
    return JsonParseResult::Ok;
  } catch (...) {
    return JsonParseResult::Oversize;
  }
}

JsonParseResult parse_user_events(
    std::string_view json, std::vector<UserEvent>& output) noexcept {
  if (json.size() > kMaximumJsonBytes) return JsonParseResult::Oversize;
  try {
    JsonValue root;
    if (!JsonParser(json).parse(root)) return JsonParseResult::Malformed;
    std::string_view channel;
    if (root.type == JsonType::Object) {
      const JsonValue* raw_channel = root.find("channel");
      if (raw_channel != nullptr && !text(raw_channel, channel))
        return JsonParseResult::InvalidValue;
    }
    const JsonValue* values = event_array(root, channel);
    if (values == nullptr || values->type != JsonType::Array)
      return JsonParseResult::MissingField;
    std::vector<UserEvent> parsed;
    parsed.reserve(values->array.size());
    for (const JsonValue& value : values->array) {
      UserEvent event;
      const bool fill = channel == "userFills" ||
                        (value.type == JsonType::Object &&
                         value.find("tid") != nullptr);
      if (!(fill ? parse_fill(value, event) : parse_order(value, event)))
        return JsonParseResult::InvalidValue;
      parsed.push_back(std::move(event));
    }
    output = std::move(parsed);
    return JsonParseResult::Ok;
  } catch (...) {
    return JsonParseResult::Oversize;
  }
}

JsonParseResult parse_exchange_response(
    std::string_view json, ExchangeResponse& output) noexcept {
  if (json.size() > kMaximumJsonBytes) return JsonParseResult::Oversize;
  try {
    JsonValue root;
    if (!JsonParser(json).parse(root) || root.type != JsonType::Object)
      return JsonParseResult::Malformed;
    ExchangeResponse parsed;
    std::string_view status;
    if (!text(root.find("status"), status)) {
      parsed.rejected = root.find("error") != nullptr;
      if (!parsed.rejected) return JsonParseResult::MissingField;
      output = std::move(parsed);
      return JsonParseResult::Ok;
    }
    if (status != "ok") {
      parsed.rejected = true;
      output = std::move(parsed);
      return JsonParseResult::Ok;
    }
    const JsonValue* item = first_status(root);
    if (item == nullptr) return JsonParseResult::MissingField;
    if (item->type == JsonType::String) {
      parsed.accepted = item->scalar == "success";
      parsed.rejected = !parsed.accepted;
      output = std::move(parsed);
      return JsonParseResult::Ok;
    }
    if (item->type != JsonType::Object) return JsonParseResult::InvalidValue;
    const JsonValue* detail = item->find("resting");
    if (detail == nullptr) detail = item->find("filled");
    if (detail == nullptr || detail->type != JsonType::Object) {
      parsed.rejected = item->find("error") != nullptr;
      output = std::move(parsed);
      return parsed.rejected ? JsonParseResult::Ok
                             : JsonParseResult::InvalidValue;
    }
    if (!copy_text(detail->find("oid"), parsed.oid))
      return JsonParseResult::MissingField;
    if (!copy_text(detail->find("cloid"), parsed.cloid))
      (void)copy_text(detail->find("cid"), parsed.cloid);
    parsed.accepted = true;
    output = std::move(parsed);
    return JsonParseResult::Ok;
  } catch (...) {
    return JsonParseResult::Oversize;
  }
}

JsonParseResult parse_open_orders(
    std::string_view json, std::vector<OpenOrder>& output) noexcept {
  if (json.size() > kMaximumJsonBytes) return JsonParseResult::Oversize;
  try {
    JsonValue root;
    if (!JsonParser(json).parse(root) || root.type != JsonType::Array)
      return JsonParseResult::Malformed;
    std::vector<OpenOrder> parsed;
    parsed.reserve(root.array.size());
    for (const JsonValue& item : root.array) {
      if (item.type != JsonType::Object)
        return JsonParseResult::InvalidValue;
      OpenOrder order;
      std::string_view side;
      if (!copy_text(item.find("coin"), order.coin) ||
          !copy_text(item.find("oid"), order.oid) ||
          !text(item.find("side"), side) ||
          !fixed_point(item.find("sz"), order.size) ||
          !fixed_point(item.find("limitPx"), order.price) ||
          !unsigned_number(item.find("timestamp"), order.time_ms) ||
          (side != "B" && side != "A"))
        return JsonParseResult::InvalidValue;
      (void)copy_text(item.find("cloid"), order.cloid);
      order.side = side == "B" ? api::Side::Buy : api::Side::Sell;
      parsed.push_back(std::move(order));
    }
    output = std::move(parsed);
    return JsonParseResult::Ok;
  } catch (...) {
    return JsonParseResult::Oversize;
  }
}

JsonParseResult parse_positions(
    std::string_view json, std::vector<Position>& output) noexcept {
  if (json.size() > kMaximumJsonBytes) return JsonParseResult::Oversize;
  try {
    JsonValue root;
    if (!JsonParser(json).parse(root) || root.type != JsonType::Object)
      return JsonParseResult::Malformed;
    const JsonValue* positions = root.find("assetPositions");
    if (positions == nullptr || positions->type != JsonType::Array)
      return JsonParseResult::MissingField;
    std::vector<Position> parsed;
    parsed.reserve(positions->array.size());
    for (const JsonValue& wrapper : positions->array) {
      if (wrapper.type != JsonType::Object)
        return JsonParseResult::InvalidValue;
      const JsonValue* raw = wrapper.find("position");
      if (raw == nullptr || raw->type != JsonType::Object)
        return JsonParseResult::MissingField;
      Position position;
      if (!copy_text(raw->find("coin"), position.coin) ||
          !fixed_point(raw->find("szi"), position.size))
        return JsonParseResult::InvalidValue;
      parsed.push_back(std::move(position));
    }
    output = std::move(parsed);
    return JsonParseResult::Ok;
  } catch (...) {
    return JsonParseResult::Oversize;
  }
}

}  // namespace oms::exchange::hyperliquid

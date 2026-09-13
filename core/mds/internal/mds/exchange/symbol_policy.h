#pragma once

#include <cstddef>
#include <cstdint>
#include <string>
#include <string_view>

namespace mds::exchange {

inline constexpr std::size_t kWireSymbolCapacity = 31;

[[nodiscard]] inline bool valid_utf8_symbol(
    std::string_view value,
    std::size_t capacity = kWireSymbolCapacity) noexcept {
  if (value.empty() || value.size() > capacity) {
    return false;
  }
  std::size_t index = 0;
  while (index < value.size()) {
    const auto first = static_cast<std::uint8_t>(value[index]);
    if (first < 0x80U) {
      if (first < 0x20U || first == 0x7fU || first == '"' || first == '\\') {
        return false;
      }
      ++index;
      continue;
    }

    std::uint32_t codepoint = 0;
    std::size_t continuation = 0;
    if (first >= 0xc2U && first <= 0xdfU) {
      codepoint = first & 0x1fU;
      continuation = 1;
    } else if (first >= 0xe0U && first <= 0xefU) {
      codepoint = first & 0x0fU;
      continuation = 2;
    } else if (first >= 0xf0U && first <= 0xf4U) {
      codepoint = first & 0x07U;
      continuation = 3;
    } else {
      return false;
    }
    if (continuation > value.size() - index - 1U) {
      return false;
    }
    for (std::size_t offset = 1; offset <= continuation; ++offset) {
      const auto next = static_cast<std::uint8_t>(value[index + offset]);
      if ((next & 0xc0U) != 0x80U) {
        return false;
      }
      codepoint = (codepoint << 6U) | (next & 0x3fU);
    }
    if ((continuation == 2U && codepoint < 0x800U) ||
        (continuation == 3U && codepoint < 0x10000U) ||
        (codepoint >= 0xd800U && codepoint <= 0xdfffU) ||
        codepoint > 0x10ffffU ||
        (codepoint >= 0x80U && codepoint <= 0x9fU)) {
      return false;
    }
    index += continuation + 1U;
  }
  return true;
}

inline void append_json_escaped(std::string &output, std::string_view value) {
  static constexpr char hex[] = "0123456789abcdef";
  for (const char raw : value) {
    const auto byte = static_cast<std::uint8_t>(raw);
    switch (byte) {
    case '"':
      output += "\\\"";
      break;
    case '\\':
      output += "\\\\";
      break;
    case '\b':
      output += "\\b";
      break;
    case '\f':
      output += "\\f";
      break;
    case '\n':
      output += "\\n";
      break;
    case '\r':
      output += "\\r";
      break;
    case '\t':
      output += "\\t";
      break;
    default:
      if (byte < 0x20U) {
        output += "\\u00";
        output.push_back(hex[byte >> 4U]);
        output.push_back(hex[byte & 0x0fU]);
      } else {
        output.push_back(raw);
      }
      break;
    }
  }
}

[[nodiscard]] inline std::string ascii_lower(std::string_view value) {
  std::string result(value);
  for (char &item : result) {
    if (item >= 'A' && item <= 'Z') {
      item = static_cast<char>(item + ('a' - 'A'));
    }
  }
  return result;
}

inline void append_url_encoded_symbol(std::string &output,
                                      std::string_view value) {
  static constexpr char hex[] = "0123456789ABCDEF";
  for (const char raw : value) {
    auto byte = static_cast<std::uint8_t>(raw);
    if (byte >= 'A' && byte <= 'Z') {
      byte = static_cast<std::uint8_t>(byte + ('a' - 'A'));
    }
    const bool unreserved =
        (byte >= 'a' && byte <= 'z') || (byte >= '0' && byte <= '9') ||
        byte == '-' || byte == '.' || byte == '_' || byte == '~';
    if (unreserved) {
      output.push_back(static_cast<char>(byte));
    } else {
      output.push_back('%');
      output.push_back(hex[byte >> 4U]);
      output.push_back(hex[byte & 0x0fU]);
    }
  }
}

[[nodiscard]] inline bool has_non_ascii(std::string_view value) noexcept {
  for (const char raw : value) {
    if (static_cast<std::uint8_t>(raw) >= 0x80U) {
      return true;
    }
  }
  return false;
}

[[nodiscard]] inline std::uint64_t stable_symbol_hash(
    std::string_view value) noexcept {
  constexpr std::uint64_t offset = 14695981039346656037ULL;
  constexpr std::uint64_t prime = 1099511628211ULL;
  std::uint64_t hash = offset;
  for (const char raw : value) {
    hash ^= static_cast<std::uint8_t>(raw);
    hash *= prime;
  }
  return hash;
}

inline void append_symbol_hash_suffix(std::string &output,
                                      std::string_view value) {
  static constexpr char hex[] = "0123456789abcdef";
  const auto hash = stable_symbol_hash(value);
  output.push_back('-');
  for (int shift = 60; shift >= 0; shift -= 4) {
    output.push_back(hex[(hash >> static_cast<unsigned>(shift)) & 0x0fU]);
  }
}

} // namespace mds::exchange

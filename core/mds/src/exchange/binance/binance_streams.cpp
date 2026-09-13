#include "mds/exchange/binance/binance_streams.h"
#include "mds/exchange/symbol_policy.h"

#include <cctype>
#include <cstring>
#include <vector>

#ifdef MDS_HAS_SIMDJSON
#include <simdjson.h>
#endif

namespace mds::exchange::binance {
namespace {

bool append_lower_symbol(std::string_view symbol, std::string &output) {
  if (!mds::exchange::valid_utf8_symbol(symbol)) {
    return false;
  }
  mds::exchange::append_url_encoded_symbol(output, symbol);
  return true;
}

bool valid_interval(std::string_view interval) {
  if (interval.empty()) {
    return false;
  }
  for (const char raw_character : interval) {
    const auto character = static_cast<unsigned char>(raw_character);
    if (!std::isalnum(character)) {
      return false;
    }
  }
  return true;
}

} // namespace

bool build_combined_stream_path(Profile profile, std::string_view symbol,
                                bool include_book_ticker,
                                bool include_depth, std::string &out,
                                std::string &error,
                                std::string_view depth_interval) {
  if (!include_book_ticker && !include_depth) {
    error = "at least one Binance stream must be requested";
    return false;
  }
  if (include_depth && !valid_interval(depth_interval)) {
    error = "invalid Binance depth interval";
    return false;
  }
  std::string lower_symbol;
  lower_symbol.reserve(symbol.size());
  if (!append_lower_symbol(symbol, lower_symbol)) {
    error = "invalid Binance stream symbol";
    return false;
  }

  std::string path(capability(profile).websocket_combined_path);
  if (include_book_ticker) {
    path += lower_symbol;
    path += "@bookTicker";
  }
  if (include_depth) {
    if (include_book_ticker) {
      path.push_back('/');
    }
    path += lower_symbol;
    path += "@depth@";
    path += depth_interval;
  }
  out = std::move(path);
  error.clear();
  return true;
}

#ifdef MDS_HAS_SIMDJSON
struct CombinedStreamParser::Impl {
  simdjson::ondemand::parser parser;
  std::vector<char> buffer;

  explicit Impl(std::size_t capacity)
      : buffer(capacity + simdjson::SIMDJSON_PADDING) {}

  simdjson::ondemand::document parse(std::string_view json) {
    if (json.size() + simdjson::SIMDJSON_PADDING > buffer.size()) {
      throw simdjson::simdjson_error(simdjson::CAPACITY);
    }
    std::memcpy(buffer.data(), json.data(), json.size());
    std::memset(buffer.data() + json.size(), 0, simdjson::SIMDJSON_PADDING);
    return parser.iterate(buffer.data(), json.size(), buffer.size());
  }
};
#endif

CombinedStreamParser::CombinedStreamParser(std::size_t capacity)
#ifdef MDS_HAS_SIMDJSON
    : impl_(new Impl(capacity))
#else
    : capacity_(capacity)
#endif
{
}

CombinedStreamParser::~CombinedStreamParser() {
#ifdef MDS_HAS_SIMDJSON
  delete impl_;
#endif
}

bool CombinedStreamParser::route(std::string_view stream, StreamRoute &out,
                                 std::string &error) noexcept {
  const auto separator = stream.find('@');
  if (separator == std::string_view::npos || separator == 0 ||
      separator + 1 == stream.size()) {
    error = "invalid Binance combined stream name";
    return false;
  }
  const auto symbol = stream.substr(0, separator);
  if (!mds::exchange::valid_utf8_symbol(symbol)) {
    error = "invalid Binance combined stream symbol";
    return false;
  }
  for (const char raw_character : symbol) {
    if (raw_character >= 'A' && raw_character <= 'Z') {
      error = "Binance combined stream symbol is not lowercase";
      return false;
    }
  }

  const auto channel = stream.substr(separator + 1);
  StreamRoute parsed;
  parsed.symbol = symbol;
  if (channel == "bookTicker") {
    parsed.kind = StreamKind::BookTicker;
  } else if (channel == "depth") {
    parsed.kind = StreamKind::Depth;
  } else if (channel.starts_with("depth@") &&
             valid_interval(channel.substr(6))) {
    parsed.kind = StreamKind::Depth;
    parsed.depth_interval = channel.substr(6);
  } else {
    error = "unsupported Binance combined stream channel";
    return false;
  }
  out = parsed;
  error.clear();
  return true;
}

bool CombinedStreamParser::unpack(std::string_view message,
                                  CombinedMessageView &out,
                                  std::string &error) {
#ifndef MDS_HAS_SIMDJSON
  (void)message;
  (void)out;
  error = "simdjson support was not compiled";
  return false;
#else
  try {
    auto document = impl_->parse(message);
    CombinedMessageView parsed;
    parsed.stream = document["stream"].get_string().value();
    parsed.data = document["data"].raw_json().value();
    if (!route(parsed.stream, parsed.route, error)) {
      return false;
    }
    out = parsed;
    error.clear();
    return true;
  } catch (const simdjson::simdjson_error &exception) {
    error = exception.what();
    return false;
  }
#endif
}

} // namespace mds::exchange::binance

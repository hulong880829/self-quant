#pragma once

#include "mds/exchange/binance/binance_adapter.h"

#include <cstddef>
#include <string>
#include <string_view>

namespace mds::exchange::binance {

enum class StreamKind : std::uint8_t { BookTicker, Depth };

struct StreamRoute {
  std::string_view symbol;
  StreamKind kind{StreamKind::Depth};
  std::string_view depth_interval;
};

struct CombinedMessageView {
  std::string_view stream;
  std::string_view data;
  StreamRoute route;
};

bool build_combined_stream_path(Profile profile, std::string_view symbol,
                                bool include_book_ticker,
                                bool include_depth, std::string &out,
                                std::string &error,
                                std::string_view depth_interval = "100ms");

class CombinedStreamParser {
public:
  explicit CombinedStreamParser(std::size_t capacity = 1U << 20U);
  ~CombinedStreamParser();
  CombinedStreamParser(const CombinedStreamParser &) = delete;
  CombinedStreamParser &operator=(const CombinedStreamParser &) = delete;

  // stream, data, and route string_views point into this parser's padded
  // buffer. They remain valid only until the next unpack() call or destruction.
  bool unpack(std::string_view message, CombinedMessageView &out,
              std::string &error);

  static bool route(std::string_view stream, StreamRoute &out,
                    std::string &error) noexcept;

private:
#ifdef MDS_HAS_SIMDJSON
  struct Impl;
  Impl *impl_{};
#else
  std::size_t capacity_{};
#endif
};

} // namespace mds::exchange::binance

#include "mds/publish/wire_publisher.h"
#include "mds/transport/shared_ring.h"
#include "utils/md/types.h"
#include "utils/md/wire.h"

#include <algorithm>
#include <array>
#include <atomic>
#include <charconv>
#include <chrono>
#include <csignal>
#include <cstdint>
#include <cstring>
#include <iostream>
#include <optional>
#include <random>
#include <string>
#include <string_view>
#include <thread>
#include <unistd.h>

namespace {

using Clock = std::chrono::steady_clock;
constexpr std::size_t kMaximumSymbols = 10;
std::atomic_bool stop_requested{};

struct Options {
  std::string segment{"/selfquant.mds.equity.ticker.1"};
  std::size_t ring_bytes{524288};
  std::size_t max_record_bytes{65536};
  std::size_t symbols{kMaximumSymbols};
  std::uint64_t min_interval_ms{1000};
  std::uint64_t max_interval_ms{3000};
  std::optional<std::uint64_t> seed{};
  std::uint64_t duration_seconds{};
};

void stop_handler(int) { stop_requested.store(true, std::memory_order_relaxed); }

template <typename T>
bool parse_unsigned(std::string_view text, T &value) {
  const auto *begin = text.data();
  const auto *end = begin + text.size();
  const auto result = std::from_chars(begin, end, value);
  return result.ec == std::errc{} && result.ptr == end;
}

bool parse_options(int argc, char **argv, Options &options) {
  for (int index = 1; index < argc; ++index) {
    const std::string_view argument{argv[index]};
    if (index + 1 >= argc) {
      return false;
    }
    const std::string_view value{argv[++index]};
    if (argument == "--segment") {
      options.segment.assign(value);
    } else if (argument == "--ring-bytes") {
      if (!parse_unsigned(value, options.ring_bytes)) {
        return false;
      }
    } else if (argument == "--max-record-bytes") {
      if (!parse_unsigned(value, options.max_record_bytes)) {
        return false;
      }
    } else if (argument == "--symbols") {
      if (!parse_unsigned(value, options.symbols)) {
        return false;
      }
    } else if (argument == "--min-interval-ms") {
      if (!parse_unsigned(value, options.min_interval_ms)) {
        return false;
      }
    } else if (argument == "--max-interval-ms") {
      if (!parse_unsigned(value, options.max_interval_ms)) {
        return false;
      }
    } else if (argument == "--seed") {
      std::uint64_t seed{};
      if (!parse_unsigned(value, seed)) {
        return false;
      }
      options.seed = seed;
    } else if (argument == "--duration") {
      if (!parse_unsigned(value, options.duration_seconds)) {
        return false;
      }
    } else {
      return false;
    }
  }
  return !options.segment.empty() && options.segment.front() == '/' &&
         options.symbols != 0 && options.symbols <= kMaximumSymbols &&
         options.min_interval_ms != 0 &&
         options.max_interval_ms >= options.min_interval_ms;
}

std::uint64_t unix_time_ns() {
  return static_cast<std::uint64_t>(
      std::chrono::duration_cast<std::chrono::nanoseconds>(
          std::chrono::system_clock::now().time_since_epoch())
          .count());
}

template <std::size_t Size>
void copy_text(std::array<char, Size> &destination, std::string_view source) {
  const auto length = std::min(source.size(), destination.size() - 1U);
  std::memcpy(destination.data(), source.data(), length);
  destination[length] = '\0';
}

utils::md::EventHeader event_header(std::uint32_t instrument_id,
                                    std::uint64_t source_sequence) {
  return {.instrument_id = instrument_id,
          .book_generation = 1,
          .source_seq = source_sequence,
          .exchange_ts_ns = unix_time_ns(),
          .state = utils::md::BookState::Live,
          .source_id = 1};
}

struct MetadataContext {
  std::array<utils::md::Instrument, kMaximumSymbols> *instruments{};
  std::size_t symbol_count{};
  std::uint64_t *source_sequence{};
};

void republish_instruments(void *raw,
                           mds::publish::WirePublisher &publisher) noexcept {
  auto &context = *static_cast<MetadataContext *>(raw);
  for (std::size_t index = 0; index < context.symbol_count; ++index) {
    const auto &instrument = (*context.instruments)[index];
    const auto header =
        event_header(instrument.instrument_id, ++*context.source_sequence);
    (void)publisher.publish_instrument(header, instrument);
  }
}

} // namespace

int main(int argc, char **argv) {
  Options options;
  if (!parse_options(argc, argv, options)) {
    std::cerr
        << "usage: equity_ticker_publisher [--segment /name] "
           "[--ring-bytes N] [--max-record-bytes N] [--symbols 1..10] "
           "[--min-interval-ms N] [--max-interval-ms N] [--seed N] "
           "[--duration seconds]\n";
    return 2;
  }
  std::signal(SIGINT, stop_handler);
  std::signal(SIGTERM, stop_handler);

  mds::transport::RingOptions ring_options;
  ring_options.name = options.segment;
  ring_options.mode = mds::api::RingMode::OverwriteOldest;
  ring_options.ring_bytes = options.ring_bytes;
  ring_options.max_record_bytes = options.max_record_bytes;
  ring_options.unlink_on_close = true;
  auto opened = mds::transport::SharedRing::open(ring_options);
  if (!opened) {
    std::cerr << "failed to create ticker segment: " << opened.message << '\n';
    return 1;
  }
  mds::publish::WirePublisher publisher(std::move(opened.value));

  std::array<utils::md::Instrument, kMaximumSymbols> instruments{};
  std::array<std::int64_t, kMaximumSymbols> open_prices{};
  std::array<std::int64_t, kMaximumSymbols> high_prices{};
  std::array<std::int64_t, kMaximumSymbols> low_prices{};
  std::array<std::int64_t, kMaximumSymbols> close_prices{};
  std::array<Clock::time_point, kMaximumSymbols> next_deadlines{};
  std::uint64_t source_sequence{};
  const auto start = Clock::now();
  const auto actual_seed = options.seed.value_or(
      unix_time_ns() ^ static_cast<std::uint64_t>(::getpid()));
  std::mt19937_64 random(actual_seed);
  std::uniform_int_distribution<std::uint64_t> interval_distribution(
      options.min_interval_ms, options.max_interval_ms);
  std::uniform_int_distribution<int> price_step(-10, 10);

  for (std::size_t index = 0; index < options.symbols; ++index) {
    auto &instrument = instruments[index];
    instrument.instrument_id = 600000U + static_cast<std::uint32_t>(index);
    instrument.venue = utils::md::Venue::Sse;
    instrument.product_type = utils::md::ProductType::Equity;
    instrument.price_scale = 2;
    instrument.quantity_scale = 0;
    instrument.tick_size = 1;
    instrument.lot_size = 100;
    const auto symbol = std::to_string(instrument.instrument_id);
    copy_text(instrument.canonical_symbol, symbol);
    copy_text(instrument.venue_symbol, symbol);
    copy_text(instrument.instrument_key, "SSE:EQUITY:" + symbol);
    copy_text(instrument.quote_asset, "CNY");
    const auto initial = 10000 + static_cast<std::int64_t>(index) * 500;
    open_prices[index] = initial;
    high_prices[index] = initial;
    low_prices[index] = initial;
    close_prices[index] = initial;
    next_deadlines[index] =
        start + std::chrono::milliseconds(interval_distribution(random));
    const auto published = publisher.publish_instrument(
        event_header(instrument.instrument_id, ++source_sequence), instrument);
    if (!published) {
      std::cerr << "failed to publish instrument " << symbol << ": "
                << published.message << '\n';
      return 1;
    }
  }

  MetadataContext metadata{&instruments, options.symbols, &source_sequence};
  publisher.set_reader_change_hook(&metadata, republish_instruments);
  const auto conservative_records =
      options.ring_bytes / options.max_record_bytes;
  const auto estimated_records =
      options.ring_bytes /
      (sizeof(mds::transport::RecordHeader) +
       sizeof(utils::md::wire::TickerRecord));
  std::cout << "equity publisher segment=" << options.segment
            << " symbols=" << options.symbols
            << " min_interval_ms=" << options.min_interval_ms
            << " max_interval_ms=" << options.max_interval_ms
            << " seed=" << actual_seed
            << " ring_bytes=" << options.ring_bytes
            << " minimum_records=" << conservative_records
            << " estimated_ticker_records=" << estimated_records << '\n';

  const auto stop_at =
      options.duration_seconds == 0
          ? Clock::time_point::max()
          : start + std::chrono::seconds(options.duration_seconds);
  while (!stop_requested.load(std::memory_order_relaxed) &&
         Clock::now() < stop_at) {
    (void)publisher.poll_reader_change();
    auto next_wakeup = stop_at;
    const auto now = Clock::now();
    for (std::size_t index = 0; index < options.symbols; ++index) {
      while (next_deadlines[index] <= now) {
        const auto next_close =
            std::max<std::int64_t>(1,
                                   close_prices[index] + price_step(random));
        close_prices[index] = next_close;
        high_prices[index] = std::max(high_prices[index], next_close);
        low_prices[index] = std::min(low_prices[index], next_close);
        utils::md::TickerEvent ticker;
        ticker.header = event_header(instruments[index].instrument_id,
                                     ++source_sequence);
        ticker.bid = {next_close - 1, 100};
        ticker.ask = {next_close + 1, 100};
        ticker.last_price = next_close;
        ticker.last_quantity = 100;
        ticker.open_price = open_prices[index];
        ticker.high_price = high_prices[index];
        ticker.low_price = low_prices[index];
        ticker.close_price = next_close;
        const auto published = publisher.publish_ticker(ticker);
        if (!published) {
          std::cerr << "ticker publish failed: " << published.message << '\n';
          return 1;
        }
        next_deadlines[index] +=
            std::chrono::milliseconds(interval_distribution(random));
      }
      next_wakeup = std::min(next_wakeup, next_deadlines[index]);
    }
    const auto wake_limit = Clock::now() + std::chrono::milliseconds(1);
    std::this_thread::sleep_until(std::min(next_wakeup, wake_limit));
  }
  return 0;
}

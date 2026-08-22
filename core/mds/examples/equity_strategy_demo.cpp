#include "mds/transport/shared_ring.h"
#include "utils/md/wire_codec.h"

#include <algorithm>
#include <array>
#include <atomic>
#include <charconv>
#include <chrono>
#include <csignal>
#include <cstdint>
#include <cstring>
#include <iomanip>
#include <iostream>
#include <optional>
#include <string>
#include <string_view>
#include <thread>
#include <unistd.h>

#if defined(__x86_64__) || defined(__i386__)
#include <immintrin.h>
#endif

namespace {

std::atomic_bool stop_requested{};

struct Options {
  std::string segment{"/selfquant.mds.equity.ticker.1"};
  std::string name{"strategy"};
  std::uint64_t process_ms{90};
};

void stop_handler(int) { stop_requested.store(true, std::memory_order_relaxed); }

void cpu_relax() noexcept {
#if defined(__x86_64__) || defined(__i386__)
  _mm_pause();
#else
  std::atomic_signal_fence(std::memory_order_seq_cst);
#endif
}

bool parse_unsigned(std::string_view text, std::uint64_t &value) {
  const auto *begin = text.data();
  const auto *end = begin + text.size();
  const auto result = std::from_chars(begin, end, value);
  return result.ec == std::errc{} && result.ptr == end;
}

bool parse_options(int argc, char **argv, Options &options) {
  for (int index = 1; index < argc; ++index) {
    if (index + 1 >= argc) {
      return false;
    }
    const std::string_view argument{argv[index]};
    const std::string_view value{argv[++index]};
    if (argument == "--segment") {
      options.segment.assign(value);
    } else if (argument == "--name") {
      options.name.assign(value);
    } else if (argument == "--process-ms") {
      if (!parse_unsigned(value, options.process_ms)) {
        return false;
      }
    } else {
      return false;
    }
  }
  return !options.segment.empty() && options.segment.front() == '/' &&
         !options.name.empty();
}

std::uint64_t unix_time_ns() {
  return static_cast<std::uint64_t>(
      std::chrono::duration_cast<std::chrono::nanoseconds>(
          std::chrono::system_clock::now().time_since_epoch())
          .count());
}

template <std::size_t Size>
std::string fixed_text(const std::array<char, Size> &value) {
  const auto end = std::find(value.begin(), value.end(), '\0');
  return {value.begin(), end};
}

std::string decimal_text(std::int64_t value, std::uint8_t scale) {
  const bool negative = value < 0;
  const auto magnitude =
      negative ? std::uint64_t{0} - static_cast<std::uint64_t>(value)
               : static_cast<std::uint64_t>(value);
  std::uint64_t divisor = 1;
  for (std::uint8_t index = 0; index < scale; ++index) {
    divisor *= 10U;
  }
  std::string result = negative ? "-" : "";
  result += std::to_string(magnitude / divisor);
  if (scale != 0) {
    result += ".";
    auto fraction = std::to_string(magnitude % divisor);
    result.append(static_cast<std::size_t>(scale) - fraction.size(), '0');
    result += fraction;
  }
  return result;
}

struct InstrumentInfo {
  std::string symbol{};
  std::uint8_t price_scale{2};
};

class Decoder final : public utils::md::wire::RecordVisitor {
public:
  bool OnInstrument(
      const utils::md::wire::InstrumentUpdateRecord &record) noexcept override {
    instrument = record;
    return true;
  }
  bool OnBbo(const utils::md::wire::BboRecord &) noexcept override {
    return true;
  }
  bool OnTicker(const utils::md::wire::TickerRecord &record) noexcept override {
    ticker = record;
    return true;
  }
  bool OnDelta(const utils::md::wire::DeltaRecord &) noexcept override {
    return true;
  }
  bool OnSnapshotBegin(
      const utils::md::wire::SnapshotBeginRecord &) noexcept override {
    return true;
  }
  bool OnSnapshotChunk(
      const utils::md::wire::SnapshotChunkRecord &) noexcept override {
    return true;
  }
  bool OnSnapshotEnd(
      const utils::md::wire::SnapshotEndRecord &) noexcept override {
    return true;
  }

  std::optional<utils::md::wire::InstrumentUpdateRecord> instrument{};
  std::optional<utils::md::wire::TickerRecord> ticker{};
};

std::size_t instrument_index(utils::md::InstrumentId instrument_id) {
  return instrument_id >= 600000U && instrument_id <= 600009U
             ? static_cast<std::size_t>(instrument_id - 600000U)
             : 10U;
}

} // namespace

int main(int argc, char **argv) {
  Options options;
  if (!parse_options(argc, argv, options)) {
    std::cerr << "usage: equity_strategy_demo [--segment /name] "
                 "[--process-ms N] [--name text]\n";
    return 2;
  }
  std::signal(SIGINT, stop_handler);
  std::signal(SIGTERM, stop_handler);

  mds::transport::RingOptions attach;
  attach.name = options.segment;
  attach.create = false;
  auto opened = mds::transport::SharedRing::open(attach);
  if (!opened) {
    std::cerr << "failed to attach ticker segment: " << opened.message << '\n';
    return 1;
  }
  auto ring = std::move(opened.value);
  auto registered = ring.register_reader(
      mds::transport::process_start_marker(
          static_cast<std::uint32_t>(::getpid())),
      unix_time_ns());
  if (!registered) {
    std::cerr << "failed to register reader: " << registered.message << '\n';
    return 1;
  }
  auto reader = registered.value;
  std::array<InstrumentInfo, 10> instruments{};
  std::uint64_t last_sequence{};
  std::uint64_t lost_records{};
  std::cout << "strategy=" << options.name << " segment=" << options.segment
            << " process_ms=" << options.process_ms
            << " start_position=LATEST idle_wait=BUSY_SPIN\n";

  while (!stop_requested.load(std::memory_order_relaxed)) {
    mds::transport::ReadLease leased;
    const auto read_error = ring.try_read(reader, leased);
    if (read_error != mds::api::ErrorCode::Ok) {
      if (read_error == mds::api::ErrorCode::QuotaExceeded) {
        cpu_relax();
        continue;
      }
      if (read_error == mds::api::ErrorCode::SubscriptionRejected ||
          read_error == mds::api::ErrorCode::RecordOverwritten) {
        if (!ring.resync_to_latest(reader)) {
          std::cerr << "strategy=" << options.name << " resync failed\n";
          return 1;
        }
        continue;
      }
      std::cerr << "strategy=" << options.name
                << " read failed: error="
                << static_cast<unsigned>(read_error) << '\n';
      return 1;
    }

    const auto ring_sequence = leased->sequence;
    Decoder decoder;
    const auto decoded = utils::md::wire::Decode(leased->payload, decoder);
    const auto committed = leased.commit();
    if (!committed) {
      if (committed.error == mds::api::ErrorCode::RecordOverwritten) {
        if (!ring.resync_to_latest(reader)) {
          std::cerr << "strategy=" << options.name << " resync failed\n";
          return 1;
        }
        continue;
      }
      std::cerr << "strategy=" << options.name
                << " commit failed: " << committed.message << '\n';
      return 1;
    }
    if (last_sequence != 0 && ring_sequence > last_sequence + 1U) {
      lost_records += ring_sequence - last_sequence - 1U;
    }
    last_sequence = ring_sequence;
    if (decoded != utils::md::wire::CodecError::Ok) {
      std::cerr << "strategy=" << options.name << " wire decode failed\n";
      continue;
    }
    if (decoder.instrument) {
      const auto index =
          instrument_index(decoder.instrument->header.instrument_id);
      if (index < instruments.size()) {
        instruments[index].symbol =
            fixed_text(decoder.instrument->instrument.canonical_symbol);
        instruments[index].price_scale =
            decoder.instrument->instrument.price_scale;
      }
      continue;
    }
    if (!decoder.ticker) {
      continue;
    }

    std::this_thread::sleep_for(
        std::chrono::milliseconds(options.process_ms));
    const auto &ticker = *decoder.ticker;
    const auto index = instrument_index(ticker.header.instrument_id);
    const auto symbol =
        index < instruments.size() && !instruments[index].symbol.empty()
            ? instruments[index].symbol
            : std::to_string(ticker.header.instrument_id);
    const auto scale =
        index < instruments.size() ? instruments[index].price_scale : 2U;
    const auto now = unix_time_ns();
    const double lag_ms =
        now >= ticker.header.exchange_ts_ns
            ? static_cast<double>(now - ticker.header.exchange_ts_ns) /
                  1'000'000.0
            : 0.0;
    std::cout << "strategy=" << options.name << " symbol=" << symbol
              << " ts_ns=" << ticker.header.exchange_ts_ns
              << " open=" << decimal_text(ticker.open_price, scale)
              << " high=" << decimal_text(ticker.high_price, scale)
              << " low=" << decimal_text(ticker.low_price, scale)
              << " close=" << decimal_text(ticker.close_price, scale)
              << " bus_seq=" << ticker.header.bus_seq << " lag_ms="
              << std::fixed << std::setprecision(3) << lag_ms
              << " lost=" << lost_records << std::endl;
  }
  (void)ring.unregister_reader(reader);
  return 0;
}

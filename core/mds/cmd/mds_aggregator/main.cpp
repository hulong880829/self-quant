#include "aggregator_config.h"

#include "mds/agg/venue_ingest.h"
#include "mds/exchange/capabilities.h"
#include "mds/publish/wire_publisher.h"
#include "utils/runtime/timestamp.h"

#include <algorithm>
#include <array>
#include <atomic>
#include <charconv>
#include <chrono>
#include <csignal>
#include <cstdint>
#include <iostream>
#include <memory>
#include <optional>
#include <string>
#include <string_view>
#include <thread>
#include <vector>
#include <unistd.h>

namespace {

std::atomic<bool> running{true};

extern "C" void request_stop(int) noexcept {
  running.store(false, std::memory_order_relaxed);
}

struct Cli {
  std::string config_path;
  std::uint64_t duration_seconds{};
  bool validate_only{};
};

void usage(std::ostream &output) {
  output << "Usage: mds_aggregator --config PATH [--validate-only] "
            "[--duration SECONDS]\n";
}

bool parse_cli(int argc, char **argv, Cli &cli) {
  for (int index = 1; index < argc; ++index) {
    const std::string_view argument(argv[index]);
    if (argument == "--validate-only") {
      cli.validate_only = true;
      continue;
    }
    if (argument == "--help" || argument == "-h") {
      return false;
    }
    if (argument != "--config" && argument != "--duration") {
      return false;
    }
    if (++index == argc) {
      return false;
    }
    const std::string_view value(argv[index]);
    if (argument == "--config") {
      cli.config_path = value;
    } else {
      const auto parsed = std::from_chars(
          value.data(), value.data() + value.size(), cli.duration_seconds);
      if (parsed.ec != std::errc{} ||
          parsed.ptr != value.data() + value.size()) {
        return false;
      }
    }
  }
  return !cli.config_path.empty();
}

template <std::size_t Size>
std::array<char, Size> fixed_text(std::string_view text) {
  std::array<char, Size> result{};
  std::copy_n(text.begin(), std::min(text.size(), Size - 1), result.begin());
  return result;
}

struct Input {
  std::size_t member_index{};
  bool orderbook{};
  bool fx{};
  mds::transport::SharedRing ring;
  mds::transport::ReaderHandle reader;
  std::unique_ptr<mds::agg::VenueIngest> ingest;
};

struct Runtime {
  const mds::aggregator::BookSpec *spec{};
  std::vector<Input> inputs;
  std::optional<mds::agg::AggregationEngine> bbo_engine;
  std::optional<mds::agg::AggregationEngine> book_engine;
  std::unique_ptr<mds::publish::WirePublisher> bbo_publisher;
  std::unique_ptr<mds::publish::WirePublisher> book_publisher;
  std::uint64_t records_read{};
  std::uint64_t ingest_errors{};
  std::uint64_t resyncs{};
  std::uint64_t bbo_published{};
  std::uint64_t book_published{};
  std::uint64_t publish_errors{};
  std::uint8_t bbo_price_scale{};
  std::uint8_t bbo_quantity_scale{};
  std::uint8_t book_price_scale{};
  std::uint8_t book_quantity_scale{};
  bool bbo_scales_frozen{};
  bool book_scales_frozen{};
  bool bbo_available{};
  bool book_available{};
};

bool open_input(Input &input, const std::string &name,
                std::uint64_t marker,
                mds::agg::VenueIngest::Selector selector = {}) {
  mds::transport::RingOptions options;
  options.name = name;
  options.create = false;
  auto opened = mds::transport::SharedRing::open(options);
  if (!opened) {
    std::cerr << "input=" << name << " open_failed=" << opened.message << '\n';
    return false;
  }
  input.ring = std::move(opened.value);
  auto reader = input.ring.register_reader(
      marker, utils::runtime::Timestamp::NowMono());
  if (!reader) {
    std::cerr << "input=" << name
              << " register_failed=" << reader.message << '\n';
    return false;
  }
  input.reader = reader.value;
  input.ingest =
      std::make_unique<mds::agg::VenueIngest>(std::move(selector));
  return true;
}

std::unique_ptr<mds::publish::WirePublisher>
open_output(mds::transport::RingOptions options, const std::string &name,
            bool aggregate_orderbook) {
  options.name = name;
  options.create = true;
  auto opened = mds::transport::SharedRing::open(options);
  if (!opened) {
    std::cerr << name << ": " << opened.message << '\n';
    return {};
  }
  auto publisher = std::make_unique<mds::publish::WirePublisher>(
      std::move(opened.value));
  if (aggregate_orderbook) {
    const auto prepared = publisher->prepare_aggregate_orderbook();
    if (!prepared) {
      std::cerr << name << ": " << prepared.message << '\n';
      return {};
    }
  }
  return publisher;
}

mds::agg::EngineConfig engine_config(
    const mds::aggregator::BookSpec &spec, std::uint8_t price_scale,
    std::uint8_t quantity_scale,
    utils::md::InstrumentId instrument_id) {
  return {.instrument_id = instrument_id,
          .base_asset = fixed_text<16>(spec.base_asset),
          .quote_asset = fixed_text<16>(spec.quote_asset),
          .price_scale = price_scale,
          .quantity_scale = quantity_scale,
          .cross_skew_observe_only = spec.cross_skew_observe_only,
          .cross_skew_threshold_us = spec.cross_skew_threshold_us,
          .fx_ttl_us = spec.fx_ttl_us,
          .fx_max_depeg_bps = spec.fx_max_depeg_bps};
}

bool valid_member_instrument(
    const utils::md::Instrument &instrument,
    const mds::aggregator::BookSpec &book,
    const mds::aggregator::MemberSpec &member) noexcept;

bool ensure_engine(Runtime &runtime, bool orderbook) {
  auto &engine = orderbook ? runtime.book_engine : runtime.bbo_engine;
  if (engine) {
    return true;
  }
  std::uint8_t price_scale = 0;
  std::uint8_t quantity_scale = 0;
  std::array<const utils::md::Instrument *, mds::agg::kMaxMembers>
      instruments{};
  for (const auto &input : runtime.inputs) {
    if (input.fx || input.orderbook != orderbook) {
      continue;
    }
    const auto *instrument = input.ingest->instrument();
    if (instrument == nullptr ||
        input.member_index >= runtime.spec->member_count ||
        !valid_member_instrument(
            *instrument, *runtime.spec,
            runtime.spec->members[input.member_index])) {
      return false;
    }
    if (instrument == nullptr) {
      continue;
    }
    instruments[input.member_index] = instrument;
    price_scale = std::max(price_scale, instrument->price_scale);
    quantity_scale = std::max(quantity_scale, instrument->quantity_scale);
  }
  for (std::size_t slot = 0; slot < runtime.spec->member_count; ++slot) {
    if (instruments[slot] == nullptr) {
      return false;
    }
  }
  auto &scales_frozen =
      orderbook ? runtime.book_scales_frozen
                : runtime.bbo_scales_frozen;
  auto &frozen_price =
      orderbook ? runtime.book_price_scale
                : runtime.bbo_price_scale;
  auto &frozen_quantity =
      orderbook ? runtime.book_quantity_scale
                : runtime.bbo_quantity_scale;
  if (!scales_frozen) {
    frozen_price = price_scale;
    frozen_quantity = quantity_scale;
    scales_frozen = true;
  } else if (price_scale > frozen_price ||
             quantity_scale > frozen_quantity) {
    return false;
  }
  engine.emplace(engine_config(*runtime.spec, frozen_price, frozen_quantity,
                               instruments[0]->instrument_id));
  for (std::size_t slot = 0; slot < runtime.spec->member_count; ++slot) {
    const auto &member = runtime.spec->members[slot];
    const auto *instrument = instruments[slot];
    const auto ttl =
        orderbook ? member.orderbook_ttl_us : member.bbo_ttl_us;
    if (!engine->add_member(
            {.venue = member.venue,
             .ttl_us = ttl,
             .price_scale = instrument->price_scale,
             .quantity_scale = instrument->quantity_scale,
             .convert_usdc_to_usdt =
                 member.source_quote_asset != runtime.spec->quote_asset})) {
      engine.reset();
      return false;
    }
  }
  for (const auto &input : runtime.inputs) {
    if (!input.fx) {
      continue;
    }
    const auto *instrument = input.ingest->instrument();
    const auto *bbo = input.ingest->bbo();
    if (instrument != nullptr && bbo != nullptr) {
      engine->update_fx(*bbo, instrument->price_scale,
                        input.ingest->bbo_ingress_ns(),
                        utils::md::Venue::Binance);
    }
  }
  return true;
}

std::string_view fixed_view(const std::array<char, 16> &text) noexcept {
  const auto end = std::find(text.begin(), text.end(), '\0');
  return {text.data(), static_cast<std::size_t>(end - text.begin())};
}

bool valid_member_instrument(
    const utils::md::Instrument &instrument,
    const mds::aggregator::BookSpec &book,
    const mds::aggregator::MemberSpec &member) noexcept {
  return instrument.venue == member.venue &&
         instrument.product_type == book.product &&
         fixed_view(instrument.base_asset) == book.base_asset &&
         fixed_view(instrument.quote_asset) == member.source_quote_asset;
}

bool valid_fx_instrument(const utils::md::Instrument &instrument) noexcept {
  return instrument.venue == utils::md::Venue::Binance &&
         instrument.product_type == utils::md::ProductType::Spot &&
         fixed_view(instrument.base_asset) == "USDC" &&
         fixed_view(instrument.quote_asset) == "USDT";
}

void print_status(const Runtime &runtime, std::string_view state) {
  std::cout << "status=" << state << " symbol=" << runtime.spec->symbol
            << " records=" << runtime.records_read
            << " ingest_errors=" << runtime.ingest_errors
            << " resyncs=" << runtime.resyncs
            << " aggbbo_published=" << runtime.bbo_published
            << " aggorderbook_published=" << runtime.book_published
            << " publish_errors=" << runtime.publish_errors << '\n';
}

utils::md::EventHeader publish_header(
    const utils::md::wire::RecordHeader &header,
    std::uint64_t now_ns) noexcept {
  utils::md::EventHeader result{};
  result.instrument_id = header.instrument_id;
  result.book_generation = header.book_generation;
  result.source_seq = header.source_seq;
  result.exchange_ts_ns = header.exchange_ts_ns;
  result.receive_tsc = 0;
  result.publish_tsc = now_ns;
  result.state = static_cast<utils::md::BookState>(header.state);
  return result;
}

void print_config(const mds::aggregator::AggregatorConfig &config) {
  for (std::size_t index = 0; index < config.book_count; ++index) {
    const auto &book = config.books[index];
    std::cout << "book symbol=" << book.symbol
              << " members=" << book.member_count
              << " aggbbo=" << (book.enable_bbo ? "on" : "off")
              << " aggorderbook="
              << (book.enable_orderbook ? "on" : "off")
              << " cross_skew="
              << (book.cross_skew_observe_only ? "observe" : "enforce")
              << " fx=" << (book.fx_enabled ? "on" : "off") << '\n';
    if (book.enable_bbo) {
      std::cout << "  output "
                << mds::aggregator::output_segment_name(config, book,
                                                        "aggbbo")
                << '\n';
    }
    if (book.enable_orderbook) {
      std::cout << "  output "
                << mds::aggregator::output_segment_name(
                       config, book, "aggorderbook")
                << '\n';
    }
    for (std::size_t slot = 0; slot < book.member_count; ++slot) {
      const auto &member = book.members[slot];
      std::cout << "  member venue="
                << mds::exchange::venue_name(member.venue)
                << " bbo_ttl_us=" << member.bbo_ttl_us
                << " book_ttl_us=" << member.orderbook_ttl_us << '\n';
    }
    if (book.fx_enabled) {
      std::cout << "  fx " << mds::aggregator::fx_segment_name(config)
                << " ttl_us=" << book.fx_ttl_us << '\n';
    }
  }
}

}  // namespace

int main(int argc, char **argv) {
  Cli cli;
  if (!parse_cli(argc, argv, cli)) {
    usage(std::cerr);
    return 2;
  }
  auto loaded = mds::aggregator::load_config(cli.config_path);
  if (!loaded) {
    std::cerr << loaded.message << '\n';
    return 2;
  }
  print_config(loaded.value);
  if (cli.validate_only) {
    return 0;
  }

  std::signal(SIGINT, request_stop);
  std::signal(SIGTERM, request_stop);
  std::signal(SIGPIPE, SIG_IGN);
  const auto marker =
      mds::transport::process_start_marker(static_cast<std::uint32_t>(getpid()));
  std::vector<Runtime> runtimes;
  runtimes.reserve(loaded.value.book_count);
  for (std::size_t index = 0; index < loaded.value.book_count; ++index) {
    const auto &spec = loaded.value.books[index];
    Runtime runtime;
    runtime.spec = &spec;
    runtime.bbo_available = spec.enable_bbo;
    runtime.book_available = spec.enable_orderbook;
    for (std::size_t slot = 0; slot < spec.member_count; ++slot) {
      const auto &member = spec.members[slot];
      const auto source_symbol =
          spec.base_asset + member.source_quote_asset;
      const mds::agg::VenueIngest::Selector selector{
          member.venue, spec.product, source_symbol};
      for (std::size_t shard = 0; shard < member.shard_count; ++shard) {
      if (runtime.bbo_available) {
        Input input;
        input.member_index = slot;
        const auto name = mds::aggregator::input_segment_name(
            loaded.value, spec, member, "ticker", shard);
        if (!open_input(input, name, marker, selector)) {
          std::cerr << name << ": unavailable; disabling AggBbo\n";
          runtime.bbo_available = false;
        } else {
          runtime.inputs.push_back(std::move(input));
        }
      }
      if (runtime.book_available) {
        Input input;
        input.member_index = slot;
        input.orderbook = true;
        const auto name = mds::aggregator::input_segment_name(
            loaded.value, spec, member, "orderbook", shard);
        if (!open_input(input, name, marker, selector)) {
          std::cerr << name << ": unavailable; disabling AggOrderBook\n";
          runtime.book_available = false;
        } else {
          runtime.inputs.push_back(std::move(input));
        }
      }
      }
    }
    if (spec.fx_enabled &&
        (runtime.bbo_available || runtime.book_available)) {
      Input input;
      input.fx = true;
      const auto name = mds::aggregator::fx_segment_name(loaded.value);
      if (!open_input(input, name, marker)) {
        std::cerr << "input=" << name
                  << " unavailable; USDC members remain excluded\n";
      } else {
        runtime.inputs.push_back(std::move(input));
      }
    }
    if (runtime.bbo_available) {
      runtime.bbo_publisher = open_output(
          loaded.value.bbo_ring,
          mds::aggregator::output_segment_name(loaded.value, spec, "aggbbo"),
          false);
      runtime.bbo_available = bool(runtime.bbo_publisher);
    }
    if (runtime.book_available) {
      runtime.book_publisher = open_output(
          loaded.value.orderbook_ring,
          mds::aggregator::output_segment_name(loaded.value, spec,
                                               "aggorderbook"),
          true);
      runtime.book_available = bool(runtime.book_publisher);
    }
    if (runtime.bbo_available || runtime.book_available) {
      std::cout << "status=started symbol=" << spec.symbol
                << " aggbbo="
                << (runtime.bbo_available ? "running" : "disabled")
                << " aggorderbook="
                << (runtime.book_available ? "running" : "disabled")
                << '\n';
      runtimes.push_back(std::move(runtime));
    }
  }
  if (runtimes.empty()) {
    std::cerr << "no aggregate output could be started\n";
    return 1;
  }

  const auto started = std::chrono::steady_clock::now();
  std::uint64_t last_heartbeat = utils::runtime::Timestamp::NowMono();
  while (running.load(std::memory_order_relaxed) &&
         (cli.duration_seconds == 0 ||
          std::chrono::steady_clock::now() - started <
              std::chrono::seconds(cli.duration_seconds))) {
    bool handled = false;
    const auto now = utils::runtime::Timestamp::NowMono();
    for (auto &runtime : runtimes) {
      for (auto &input : runtime.inputs) {
        mds::transport::ReadLease lease;
        const auto error = input.ring.try_read(input.reader, lease);
        if (error != mds::api::ErrorCode::Ok) {
          if (error == mds::api::ErrorCode::SubscriptionRejected ||
              error == mds::api::ErrorCode::RecordOverwritten) {
            const auto resynced = input.ring.resync_to_latest(input.reader);
            input.ingest->reset();
            ++runtime.resyncs;
            if (!resynced) {
              ++runtime.ingest_errors;
            }
            if (input.fx) {
              runtime.bbo_engine.reset();
              runtime.book_engine.reset();
            } else if (input.orderbook) {
              if (runtime.book_engine) {
                runtime.book_engine->invalidate_member(input.member_index);
              }
            } else {
              if (runtime.bbo_engine) {
                runtime.bbo_engine->invalidate_member(input.member_index);
              }
            }
          } else if (error != mds::api::ErrorCode::QuotaExceeded) {
            ++runtime.ingest_errors;
          }
          continue;
        }
        const auto view = lease.view();
        const auto result = input.ingest->consume(
            view.sequence, view.type, view.payload, now);
        (void)lease.commit();
        handled = true;
        ++runtime.records_read;
        if (result == mds::agg::IngestResult::Invalid ||
            result == mds::agg::IngestResult::NeedResync) {
          ++runtime.ingest_errors;
        }
        if (result == mds::agg::IngestResult::NeedResync) {
          (void)input.ring.resync_to_latest(input.reader);
          ++runtime.resyncs;
          if (input.fx) {
            runtime.bbo_engine.reset();
            runtime.book_engine.reset();
          } else if (input.orderbook) {
            if (runtime.book_engine) {
              runtime.book_engine->invalidate_member(input.member_index);
            }
          } else if (runtime.bbo_engine) {
            runtime.bbo_engine->invalidate_member(input.member_index);
          }
          continue;
        }
        if (result == mds::agg::IngestResult::Instrument) {
          const auto *instrument = input.ingest->instrument();
          if (input.fx) {
            if (runtime.bbo_engine) {
              runtime.bbo_engine->invalidate_fx();
            }
            if (runtime.book_engine) {
              runtime.book_engine->invalidate_fx();
            }
            if (instrument == nullptr ||
                !valid_fx_instrument(*instrument)) {
              ++runtime.ingest_errors;
            }
            (void)ensure_engine(runtime, false);
            (void)ensure_engine(runtime, true);
            continue;
          }
          auto &engine = input.orderbook ? runtime.book_engine
                                         : runtime.bbo_engine;
          if (engine) {
            const auto &member =
                runtime.spec->members[input.member_index];
            const auto ttl = input.orderbook
                                 ? member.orderbook_ttl_us
                                 : member.bbo_ttl_us;
            const bool metadata_valid =
                instrument != nullptr &&
                valid_member_instrument(*instrument, *runtime.spec,
                                        member);
            if (!metadata_valid) {
              engine->exclude_member(input.member_index);
              ++runtime.ingest_errors;
            } else if (!engine->reconfigure_member(
                    input.member_index,
                    {.venue = member.venue,
                     .ttl_us = ttl,
                     .price_scale = instrument->price_scale,
                     .quantity_scale = instrument->quantity_scale,
                     .convert_usdc_to_usdt =
                         member.source_quote_asset !=
                         runtime.spec->quote_asset})) {
              ++runtime.ingest_errors;
            }
          }
          (void)ensure_engine(runtime, input.orderbook);
          continue;
        }
        if (input.fx) {
          const auto *instrument = input.ingest->instrument();
          const auto *bbo = input.ingest->bbo();
          if (instrument != nullptr && bbo != nullptr &&
              valid_fx_instrument(*instrument)) {
            if (runtime.bbo_engine) {
              runtime.bbo_engine->update_fx(
                  *bbo, instrument->price_scale,
                  input.ingest->bbo_ingress_ns(),
                  utils::md::Venue::Binance);
            }
            if (runtime.book_engine) {
              runtime.book_engine->update_fx(
                  *bbo, instrument->price_scale,
                  input.ingest->bbo_ingress_ns(),
                  utils::md::Venue::Binance);
            }
          } else if (result == mds::agg::IngestResult::Instrument &&
                     instrument != nullptr &&
                     !valid_fx_instrument(*instrument)) {
            ++runtime.ingest_errors;
          }
          continue;
        }
        const auto *instrument = input.ingest->instrument();
        if (instrument != nullptr &&
            !valid_member_instrument(
                *instrument, *runtime.spec,
                runtime.spec->members[input.member_index])) {
          if (result == mds::agg::IngestResult::Instrument) {
            ++runtime.ingest_errors;
          }
          continue;
        }
        if (!ensure_engine(runtime, input.orderbook)) {
          continue;
        }
        auto &engine =
            input.orderbook ? runtime.book_engine : runtime.bbo_engine;
        if (input.orderbook) {
          mds::agg::BookInput book_input;
          if (input.ingest->book_input(book_input)) {
            (void)engine->update_book(input.member_index, book_input);
          }
        } else if (const auto *bbo = input.ingest->bbo()) {
          (void)engine->update_bbo(input.member_index, *bbo,
                                   input.ingest->bbo_ingress_ns());
        }
      }

      if (runtime.bbo_engine && runtime.bbo_publisher) {
        const auto built = runtime.bbo_engine->build_bbo(now);
        if (built.changed) {
          const auto published = runtime.bbo_publisher->publish_agg_bbo(
              publish_header(built.record.header, now), built.record,
              built.flags);
          if (published) {
            ++runtime.bbo_published;
          } else {
            ++runtime.publish_errors;
          }
          if (built.member_data_error) {
            ++runtime.ingest_errors;
          }
        }
      }
      if (runtime.book_engine && runtime.book_publisher) {
        const auto built = runtime.book_engine->build_orderbook(now);
        if (built.changed) {
          const auto published = runtime.book_publisher->publish_agg_orderbook(
              publish_header(built.record.header, now), built.record);
          if (published) {
            ++runtime.book_published;
          } else {
            ++runtime.publish_errors;
          }
        }
      }
    }
    if (now - last_heartbeat >= 1'000'000'000ULL) {
      for (auto &runtime : runtimes) {
        for (auto &input : runtime.inputs) {
          (void)input.ring.heartbeat(input.reader, now);
        }
      }
      last_heartbeat = now;
    }
    if (!handled) {
      std::this_thread::sleep_for(std::chrono::milliseconds(1));
    }
  }

  for (auto &runtime : runtimes) {
    for (auto &input : runtime.inputs) {
      (void)input.ring.unregister_reader(input.reader);
    }
    print_status(runtime, "stopped");
  }
  return 0;
}

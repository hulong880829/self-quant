#include "mds/exchange/binance/binance_venue_adapter.h"

#include "mds/exchange/binance/binance_adapter.h"
#include "mds/exchange/binance/binance_rest.h"
#include "mds/exchange/symbol_policy.h"

#include <algorithm>
#include <cctype>
#include <cstring>
#include <memory>
#include <string>
#include <utility>
#include <vector>

#ifdef MDS_HAS_SIMDJSON
#include <simdjson.h>
#endif

namespace mds::exchange {
namespace {

using Profile = binance::Profile;
constexpr std::size_t kMetadataTargetCapacity = 6U << 10U;

Profile profile(utils::md::ProductType product) noexcept {
  return product == utils::md::ProductType::Spot ? Profile::Spot
                                                  : Profile::UsdM;
}

class BinanceVenueAdapter final : public VenueAdapter {
 public:
  struct SymbolState {
    std::string canonical;
    std::string venue;
    binance::InstrumentMetadata metadata;
    binance::DepthUpdate depth;
    binance::DepthSnapshot snapshot;

    SymbolState(std::string canonical_symbol, std::string venue_symbol,
                binance::InstrumentMetadata parsed,
                std::size_t maximum_levels)
        : canonical(std::move(canonical_symbol)),
          venue(std::move(venue_symbol)),
          metadata(std::move(parsed)) {
      depth.bids.reserve(maximum_levels);
      depth.asks.reserve(maximum_levels);
      snapshot.bids.reserve(maximum_levels);
      snapshot.asks.reserve(maximum_levels);
    }
  };

  BinanceVenueAdapter(utils::md::ProductType product,
                      std::size_t maximum_levels)
      : product_(product), profile_(profile(product)),
        maximum_levels_(maximum_levels), ws_parser_(maximum_levels)
#ifdef MDS_HAS_SIMDJSON
        ,
        route_buffer_(1U << 20U)
#endif
  {
#ifdef MDS_HAS_SIMDJSON
    const auto allocated = route_parser_.allocate(1U << 20U);
    (void)allocated;
    const auto warm_size =
        route_buffer_.size() - simdjson::SIMDJSON_PADDING;
    std::memset(route_buffer_.data(), 'x', warm_size);
    constexpr std::string_view prefix = R"({"_":")";
    std::memcpy(route_buffer_.data(), prefix.data(), prefix.size());
    route_buffer_[warm_size - 2] = '"';
    route_buffer_[warm_size - 1] = '}';
    const auto warmed =
        route_parser_.parse(route_buffer_.data(), warm_size, false);
    (void)warmed.error();
#endif
  }

  utils::md::Venue venue() const noexcept override {
    return utils::md::Venue::Binance;
  }
  utils::md::ProductType product() const noexcept override {
    return product_;
  }
  HeartbeatSpec heartbeat() const override {
    return {HeartbeatKind::Rfc6455Ping, {}, 30'000};
  }

  bool build_subscription_batches(
      std::span<const StreamRequest> requests,
      std::vector<std::string> &batches, std::string &error) const override {
    std::vector<std::string> topics;
    topics.reserve(requests.size() * 2);
    for (const auto &request : requests) {
      if (!valid_utf8_symbol(request.venue_symbol)) {
        continue;
      }
      const auto symbol = ascii_lower(request.venue_symbol);
      if (request.ticker) {
        if (!request.ticker_channel.empty() &&
            request.ticker_channel != "bookTicker") {
          error = "unsupported Binance ticker channel";
          return false;
        }
        topics.push_back(symbol + "@bookTicker");
      }
      if (request.orderbook) {
        if (!request.orderbook_channel.empty() &&
            request.orderbook_channel != "depth") {
          error = "unsupported Binance orderbook channel";
          return false;
        }
        const auto interval =
            request.update_interval_ms == 0 ? 100
                                            : request.update_interval_ms;
        topics.push_back(symbol + "@depth@" + std::to_string(interval) +
                         "ms");
      }
    }
    if (topics.empty()) {
      error = "at least one Binance stream must be requested";
      return false;
    }
    batches.clear();
    constexpr std::size_t max_topics_per_request = 200;
    for (std::size_t offset = 0; offset < topics.size();
         offset += max_topics_per_request) {
      std::string batch = R"({"method":"SUBSCRIBE","params":[)";
      const auto end =
          std::min(topics.size(), offset + max_topics_per_request);
      for (std::size_t index = offset; index < end; ++index) {
        if (index != offset) {
          batch.push_back(',');
        }
        batch.push_back('"');
        append_json_escaped(batch, topics[index]);
        batch.push_back('"');
      }
      batch.append(R"(],"id":)");
      batch.append(std::to_string(batches.size() + 1));
      batch.push_back('}');
      batches.push_back(std::move(batch));
    }
    error.clear();
    return true;
  }

  bool build_unsubscription_batches(
      std::span<const StreamRequest> requests,
      std::vector<std::string> &batches,
      std::string &error) const override {
    if (!build_subscription_batches(requests, batches, error)) {
      return false;
    }
    for (auto &batch : batches) {
      constexpr std::string_view subscribe{"\"SUBSCRIBE\""};
      const auto operation = batch.find(subscribe);
      if (operation == std::string::npos) {
        error =
            "Binance subscription cannot be converted to unsubscribe";
        batches.clear();
        return false;
      }
      batch.replace(operation, subscribe.size(), "\"UNSUBSCRIBE\"");
    }
    error.clear();
    return true;
  }

  bool parse_ws(std::string_view json, NormalizedEvent &event,
                std::string &error) override {
    event.reset();
#ifndef MDS_HAS_SIMDJSON
    (void)json;
    error = "simdjson support was not compiled";
    return false;
#else
    try {
      auto document = route(json);
      auto code = document["code"].get_int64();
      if (!code.error()) {
        event.type = AdapterEventType::SubscribeError;
        error = "Binance subscription rejected";
        auto message = document["msg"].get_string();
        if (!message.error()) {
          error += ": ";
          error.append(message.value());
        }
        return true;
      }
      auto result = document["result"];
      if (!result.error()) {
        event.type = AdapterEventType::SubscribeAck;
        error.clear();
        return true;
      }
      auto event_name_result = document["e"].get_string();
      auto symbol_result = document["s"].get_string();
      if (symbol_result.error()) {
        error.clear();
        return true;
      }
      std::string_view event_name;
      if (!event_name_result.error()) {
        event_name = event_name_result.value();
      } else if (!document["b"].error() && !document["a"].error()) {
        event_name = "bookTicker";
      } else {
        error.clear();
        return true;
      }
      const std::string_view symbol = symbol_result.value();
      auto *state = find(symbol);
      if (state == nullptr || !copy_symbol(symbol, event)) {
        error = "Binance event references an unknown symbol: ";
        error.append(symbol);
        return false;
      }
      if (event_name == "bookTicker") {
        binance::BookTicker ticker;
        if (!ws_parser_.parse_book_ticker(
                json, state->metadata.price_scale,
                state->metadata.quantity_scale, ticker, error)) {
          return false;
        }
        event.type = AdapterEventType::Bbo;
        event.first_sequence = ticker.update_id;
        event.final_sequence = ticker.update_id;
        event.exchange_time_ms =
            ticker.event_time_ms != 0 ? ticker.event_time_ms
                                      : ticker.transaction_time_ms;
        event.bid = {ticker.bid_price, ticker.bid_quantity};
        event.ask = {ticker.ask_price, ticker.ask_quantity};
        error.clear();
        return true;
      }
      if (event_name == "depthUpdate") {
        const auto parse_result = ws_parser_.parse_depth_classified(
            json, state->metadata.price_scale,
            state->metadata.quantity_scale, state->depth, error);
        if (parse_result == binance::DepthParseResult::Invalid) {
          return false;
        }
        event.first_sequence = state->depth.first_update_id;
        event.final_sequence = state->depth.final_update_id;
        event.previous_sequence =
            state->depth.previous_final_update_id;
        event.strict_previous_sequence =
            profile_ == Profile::UsdM &&
            state->depth.previous_final_update_id != 0;
        event.exchange_time_ms =
            state->depth.event_time_ms != 0
                ? state->depth.event_time_ms
                : state->depth.transaction_time_ms;
        if (parse_result ==
            binance::DepthParseResult::CapacityExceeded) {
          event.type = AdapterEventType::BookGap;
          event.input_side =
              state->depth.capacity_side == binance::DepthSide::Bid
                  ? InputSide::Bid
                  : InputSide::Ask;
          event.input_capacity = state->depth.capacity_limit;
          return true;
        }
        event.type = AdapterEventType::BookDelta;
        if (state->depth.bids.size() > event.bids.capacity() ||
            state->depth.asks.size() > event.asks.capacity()) {
          event.type = AdapterEventType::BookGap;
          event.input_side =
              state->depth.bids.size() > event.bids.capacity()
                  ? InputSide::Bid
                  : InputSide::Ask;
          event.input_capacity =
              event.input_side == InputSide::Bid
                  ? event.bids.capacity()
                  : event.asks.capacity();
          state->depth.bids.clear();
          state->depth.asks.clear();
          error = "Binance depth update exceeds normalized event capacity";
          return true;
        }
        event.bids.insert(event.bids.end(), state->depth.bids.begin(),
                          state->depth.bids.end());
        event.asks.insert(event.asks.end(), state->depth.asks.begin(),
                          state->depth.asks.end());
        error.clear();
        return true;
      }
      error.clear();
      return true;
    } catch (const simdjson::simdjson_error &exception) {
      error = exception.what();
      return false;
    }
#endif
  }

  HttpRequestSpec metadata_request() const override {
    return {HttpRequestSpec::Method::Get,
            profile_ == Profile::Spot ? "/api/v3/exchangeInfo"
                                      : "/fapi/v1/exchangeInfo",
            {}, {}};
  }

  HttpRequestSpec metadata_request(
      std::span<const StreamRequest> requests) const override {
    auto request = metadata_request();
    if (profile_ != Profile::Spot || requests.empty()) {
      return request;
    }
    request.target.append("?symbols=%5B");
    for (std::size_t index = 0; index < requests.size(); ++index) {
      if (index != 0) {
        request.target.append("%2C");
      }
      request.target.append("%22");
      request.target.append(requests[index].venue_symbol);
      request.target.append("%22");
      if (request.target.size() + std::string_view("%5D").size() >
          kMetadataTargetCapacity) {
        return metadata_request();
      }
    }
    request.target.append("%5D");
    return request;
  }

  bool parse_metadata(std::string_view json,
                      std::span<const StreamRequest> requests,
                      std::vector<InstrumentMetadata> &metadata,
                      std::string &error) override {
    states_.clear();
    std::vector<binance::InstrumentMetadata> parsed_metadata;
    if (!parse_metadata_values(json, requests, parsed_metadata, metadata,
                               false, error)) {
      return false;
    }
    states_.reserve(requests.size());
    for (std::size_t index = 0; index < requests.size(); ++index) {
      const auto &request = requests[index];
      auto &parsed = parsed_metadata[index];
      if (parsed.venue_symbol.empty()) {
        continue;
      }
      auto venue_symbol = parsed.venue_symbol;
      states_.emplace_back(std::string(request.canonical_symbol),
                           std::move(venue_symbol), std::move(parsed),
                           maximum_levels_);
    }
    error.clear();
    return true;
  }

  bool upsert_metadata(
      std::string_view json, std::span<const StreamRequest> requests,
      std::vector<InstrumentMetadata> &metadata,
      std::string &error) override {
    auto existing = std::move(states_);
    std::vector<InstrumentMetadata> parsed;
    const bool ok = parse_metadata(json, requests, parsed, error);
    auto refreshed = std::move(states_);
    states_ = std::move(existing);
    if (!ok) {
      return false;
    }
    for (auto &entry : refreshed) {
      const auto found = std::find_if(
          states_.begin(), states_.end(),
          [&entry](const SymbolState &current) {
            return current.venue == entry.venue;
          });
      if (found == states_.end()) {
        states_.push_back(std::move(entry));
      } else {
        *found = std::move(entry);
      }
    }
    metadata = std::move(parsed);
    error.clear();
    return true;
  }

  bool parse_discovery_metadata(
      std::string_view json, std::span<const StreamRequest> requests,
      std::vector<InstrumentMetadata> &metadata,
      std::string &error) override {
    std::vector<binance::InstrumentMetadata> parsed_metadata;
    return parse_metadata_values(json, requests, parsed_metadata, metadata,
                                 true, error);
  }

  [[nodiscard]] HttpRequestSpec
  discovery_turnover_request() const override {
    return {HttpRequestSpec::Method::Get,
            profile_ == Profile::Spot ? "/api/v3/ticker/24hr"
                                      : "/fapi/v1/ticker/24hr",
            {}, {}};
  }

  bool enrich_discovery_turnover(
      std::string_view json, std::span<InstrumentMetadata> metadata,
      std::string &error) override {
#ifndef MDS_HAS_SIMDJSON
    (void)json;
    (void)metadata;
    error =
        "Binance 24h turnover parsing requires simdjson support";
    return false;
#else
    try {
      simdjson::dom::parser turnover_parser;
      simdjson::padded_string padded(json);
      auto root = turnover_parser.parse(padded).value();
      auto entries_result = root.get_array();
      if (entries_result.error()) {
        auto object_result = root.get_object();
        if (!object_result.error() &&
            !object_result.value()["code"].error()) {
          error = "Binance 24h ticker request failed";
          auto message =
              object_result.value()["msg"].get_string();
          if (!message.error()) {
            error += ": ";
            error.append(message.value());
          }
        } else {
          error = "Binance 24h ticker response is not an array";
        }
        return false;
      }

      std::vector<std::uint64_t> turnovers(metadata.size());
      std::vector<bool> matched(metadata.size(), false);
      for (auto raw_entry : entries_result.value()) {
        auto entry_result = raw_entry.get_object();
        if (entry_result.error()) {
          continue;
        }
        auto entry = entry_result.value();
        auto symbol_result = entry["symbol"].get_string();
        if (symbol_result.error()) {
          continue;
        }
        const std::string_view symbol = symbol_result.value();
        const auto found = std::find_if(
            metadata.begin(), metadata.end(),
            [symbol](const InstrumentMetadata &instrument) {
              return instrument.venue_symbol == symbol;
            });
        if (found == metadata.end()) {
          continue;
        }
        const auto index =
            static_cast<std::size_t>(found - metadata.begin());
        if (matched[index]) {
          error = "duplicate Binance 24h ticker symbol: ";
          error.append(symbol);
          return false;
        }
        auto turnover_result = entry["quoteVolume"].get_string();
        if (turnover_result.error() ||
            !decimal_to_turnover(turnover_result.value(),
                                 turnovers[index])) {
          error = "invalid Binance 24h quote turnover for symbol: ";
          error.append(symbol);
          return false;
        }
        matched[index] = true;
      }
      for (std::size_t index = 0; index < metadata.size(); ++index) {
        metadata[index].turnover_24h = turnovers[index];
      }
      error.clear();
      return true;
    } catch (const simdjson::simdjson_error &exception) {
      error = exception.what();
      return false;
    }
#endif
  }

  bool needs_rest_snapshot() const noexcept override { return true; }

  HttpRequestSpec snapshot_request(std::string_view venue_symbol,
                                   std::size_t depth) const override {
    const auto &capability = binance::capability(profile_);
    std::string target(capability.depth_path);
    target.append("?symbol=");
    target.append(venue_symbol);
    target.append("&limit=");
    target.append(std::to_string(depth));
    return {HttpRequestSpec::Method::Get, std::move(target), {}, {}};
  }

  bool parse_snapshot(std::string_view json, std::string_view venue_symbol,
                      NormalizedEvent &event,
                      std::string &error) override {
    event.reset();
    auto *state = find(venue_symbol);
    if (state == nullptr || !copy_symbol(venue_symbol, event)) {
      error = "Binance snapshot references an unknown symbol";
      return false;
    }
    state->snapshot.bids.clear();
    state->snapshot.asks.clear();
    if (!rest_parser_.parse_depth(profile_, json, state->metadata,
                                  state->snapshot, error)) {
      error = "Binance snapshot symbol=" + std::string(venue_symbol) +
              ": " + error;
      return false;
    }
    if (state->snapshot.bids.size() > event.bids.capacity() ||
        state->snapshot.asks.size() > event.asks.capacity()) {
      error = "Binance snapshot symbol=" + std::string(venue_symbol) +
              ": event capacity";
      return false;
    }
    event.type = AdapterEventType::BookSnapshot;
    event.first_sequence = state->snapshot.last_update_id;
    event.final_sequence = state->snapshot.last_update_id;
    event.bids.insert(event.bids.end(), state->snapshot.bids.begin(),
                      state->snapshot.bids.end());
    event.asks.insert(event.asks.end(), state->snapshot.asks.begin(),
                      state->snapshot.asks.end());
    event.sequence_reset = true;
    error.clear();
    return true;
  }

 protected:
  void classify_parse_failure(std::string_view,
                              const NormalizedEvent &event,
                              ParseFailure &failure) const noexcept override {
    (void)classify_scale_mismatch(event, failure);
  }

 private:
  bool parse_metadata_values(
      std::string_view json, std::span<const StreamRequest> requests,
      std::vector<binance::InstrumentMetadata> &parsed_metadata,
      std::vector<InstrumentMetadata> &metadata, bool trading_only,
      std::string &error) {
    metadata.clear();
    std::vector<std::string_view> venue_symbols;
    venue_symbols.reserve(requests.size());
    for (const auto &request : requests) {
      venue_symbols.push_back(request.venue_symbol);
    }
    binance::RestParser metadata_parser(32U << 20U);
    if (!metadata_parser.parse_exchange_info(
            profile_, json, venue_symbols, parsed_metadata, error)) {
      return false;
    }
    metadata.reserve(requests.size());
    for (std::size_t index = 0; index < requests.size(); ++index) {
      const auto &request = requests[index];
      const auto &parsed = parsed_metadata[index];
      if (parsed.venue_symbol.empty()) {
        continue;
      }
      if (trading_only &&
          (parsed.status != "TRADING" ||
           (profile_ == Profile::UsdM &&
            parsed.contract_type != "PERPETUAL" &&
            parsed.contract_type != "TRADIFI_PERPETUAL"))) {
        continue;
      }
      if (parsed.price_scale < 0 || parsed.quantity_scale < 0) {
        error = "invalid Binance instrument scale";
        return false;
      }
      InstrumentMetadata instrument;
      instrument.canonical_symbol = request.canonical_symbol;
      instrument.venue_symbol = parsed.venue_symbol;
      instrument.base_asset = parsed.base_asset;
      instrument.quote_asset = parsed.quote_asset;
      instrument.settle_asset = parsed.settle_asset;
      instrument.price_scale =
          static_cast<std::uint8_t>(parsed.price_scale);
      instrument.quantity_scale =
          static_cast<std::uint8_t>(parsed.quantity_scale);
      instrument.tick_size = parsed.price_filter.tick_size;
      instrument.lot_size = parsed.lot_size.step_size;
      instrument.contract_multiplier = parsed.contract_size;
      instrument.refine_book_tick = profile_ == Profile::UsdM;
      if (instrument.tick_size <= 0 || instrument.lot_size <= 0) {
        error = "invalid Binance instrument increments";
        return false;
      }
      metadata.push_back(std::move(instrument));
    }
    error.clear();
    return true;
  }

  SymbolState *find(std::string_view venue_symbol) noexcept {
    const auto found = std::find_if(
        states_.begin(), states_.end(),
        [venue_symbol](const SymbolState &entry) {
          if (entry.venue.size() != venue_symbol.size()) {
            return false;
          }
          for (std::size_t index = 0; index < venue_symbol.size();
               ++index) {
            if (std::toupper(static_cast<unsigned char>(
                    entry.venue[index])) !=
                std::toupper(static_cast<unsigned char>(
                    venue_symbol[index]))) {
              return false;
            }
          }
          return true;
        });
    return found == states_.end() ? nullptr : &*found;
  }

#ifdef MDS_HAS_SIMDJSON
  simdjson::dom::element route(std::string_view json) {
    if (json.size() + simdjson::SIMDJSON_PADDING >
        route_buffer_.size()) {
      throw simdjson::simdjson_error(simdjson::CAPACITY);
    }
    std::memcpy(route_buffer_.data(), json.data(), json.size());
    std::memset(route_buffer_.data() + json.size(), 0,
                simdjson::SIMDJSON_PADDING);
    return route_parser_.parse(route_buffer_.data(), json.size(), false)
        .value();
  }
#endif

  utils::md::ProductType product_;
  Profile profile_;
  std::size_t maximum_levels_;
  binance::JsonParser ws_parser_;
  binance::RestParser rest_parser_;
  std::vector<SymbolState> states_;
#ifdef MDS_HAS_SIMDJSON
  simdjson::dom::parser route_parser_;
  std::vector<char> route_buffer_;
#endif
};

}  // namespace

std::unique_ptr<VenueAdapter>
make_binance_venue_adapter(utils::md::ProductType product,
                           std::size_t max_levels_per_side) {
  if ((product != utils::md::ProductType::Spot &&
       product != utils::md::ProductType::Perpetual) ||
      max_levels_per_side == 0 ||
      max_levels_per_side >
          binance::kDefaultMaxDepthLevelsPerSide) {
    return nullptr;
  }
  return std::make_unique<BinanceVenueAdapter>(
      product, max_levels_per_side);
}

}  // namespace mds::exchange

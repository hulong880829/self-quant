#include "fairprice/bootstrap.h"
#include "fairprice/catalog.h"
#include "fairprice/config.h"
#include "fairprice/engine.h"
#include "fairprice/history.h"
#include "fairprice/json.h"
#include "fairprice/log.h"
#include "fairprice/log_summary.h"
#include "fairprice/recorder.h"
#include "fairprice/market_data_repository.h"
#include "fairprice/runtime.h"
#include "fairprice/sqab.h"
#include "fairprice/store.h"
#include "mds/api/mds_api.h"
#include "utils/md/wire_codec.h"

#include <algorithm>
#include <atomic>
#include <chrono>
#include <cstddef>
#include <ctime>
#include <cstdint>
#include <cstdio>
#include <cstring>
#include <exception>
#include <filesystem>
#include <fstream>
#include <stdexcept>
#include <string>
#include <thread>
#include <unistd.h>
#include <vector>

#include <simdjson.h>

namespace {

void require(bool condition, const char *message) {
  if (!condition) {
    throw std::runtime_error(message);
  }
}

std::string catalog_json_row(std::string_view profile, std::string_view symbol,
                             std::string_view kind, std::string_view segment,
                             std::uint64_t updated_at_ns) {
  return std::string("{\"profile\":\"") + std::string(profile) +
         "\",\"symbol\":\"" + std::string(symbol) + "\",\"kind\":\"" +
         std::string(kind) + "\",\"segment\":\"" + std::string(segment) +
         "\",\"active\":1,\"schema_major\":2,\"schema_minor\":0,\"updated_at_ns\":" +
         std::to_string(updated_at_ns) + "}";
}

fairprice::ClickHouseReadConfig test_ch_config() {
  fairprice::ClickHouseReadConfig config;
  config.host = "127.0.0.1";
  config.catalog_scope = "prod-agg";
  return config;
}

fairprice::MarketDataQuery sql_body_query(std::string catalog,
                                          std::string latest_bbo = {},
                                          std::string latest_book = {},
                                          std::string history = {},
                                          std::string ohlc = {},
                                          std::string distribution = {}) {
  return [catalog = std::move(catalog), latest_bbo = std::move(latest_bbo),
          latest_book = std::move(latest_book), history = std::move(history),
          ohlc = std::move(ohlc),
          distribution = std::move(distribution)](std::string_view sql) {
    fairprice::MarketDataQueryResult result;
    const auto text = std::string(sql);
    if (text.find("aggregate_topic_catalog") != std::string::npos) {
      result.body = catalog;
    } else if (text.find("bid_count") != std::string::npos &&
               text.find("LIMIT 1 BY segment") != std::string::npos) {
      result.body = latest_book;
    } else if (text.find("LIMIT 1 BY segment") != std::string::npos) {
      result.body = latest_bbo;
    } else if (text.find("GROUP BY bucket_start_ns") != std::string::npos) {
      result.body = ohlc.empty() ? history : ohlc;
    } else if (text.find("arrayJoin") != std::string::npos) {
      result.body = distribution.empty() ? history : distribution;
    } else {
      result.body = history;
    }
    return result;
  };
}

fairprice::CatalogSettings test_catalog_settings() {
  return {.sample_interval_ms = 200, .retention_hours = 24, .depth = 50};
}

utils::md::wire::AggBboRecord test_bbo_record(const char *base, const char *quote,
                                              std::int64_t bid, std::int64_t ask) {
  utils::md::wire::AggBboRecord record{};
  std::memcpy(record.base_asset.data(), base, std::strlen(base));
  std::memcpy(record.quote_asset.data(), quote, std::strlen(quote));
  record.member_count = 1;
  record.member_mask = 1;
  record.live_mask = 1;
  record.venue_slot_ids[0] = 1;
  record.gated_bid.price = bid;
  record.gated_bid.quantity = 10;
  record.gated_bid.venue_quantity[0] = 10;
  record.gated_bid.venue_mask = 1;
  record.gated_bid.contributor_count = 1;
  record.gated_ask.price = ask;
  record.gated_ask.quantity = 10;
  record.gated_ask.venue_quantity[0] = 10;
  record.gated_ask.venue_mask = 1;
  record.gated_ask.contributor_count = 1;
  record.raw_bid.price = bid;
  record.raw_bid.quantity = 10;
  record.raw_bid.venue_mask = 1;
  record.raw_ask.price = ask;
  record.raw_ask.quantity = 10;
  record.raw_ask.venue_mask = 1;
  return record;
}

utils::md::wire::AggOrderBookRecord test_book_record(const char *base,
                                                     const char *quote,
                                                     std::int64_t bid,
                                                     std::int64_t ask) {
  utils::md::wire::AggOrderBookRecord record{};
  std::memcpy(record.base_asset.data(), base, std::strlen(base));
  std::memcpy(record.quote_asset.data(), quote, std::strlen(quote));
  record.member_count = 1;
  record.member_mask = 1;
  record.active_mask = 1;
  record.venue_slot_ids[0] = 1;
  record.bid_count = 1;
  record.ask_count = 1;
  record.bids[0].price = bid;
  record.bids[0].quantity = 10;
  record.bids[0].venue_quantity[0] = 10;
  record.bids[0].venue_mask = 1;
  record.bids[0].contributor_count = 1;
  record.asks[0].price = ask;
  record.asks[0].quantity = 10;
  record.asks[0].venue_quantity[0] = 10;
  record.asks[0].venue_mask = 1;
  record.asks[0].contributor_count = 1;
  return record;
}

std::string read_file(const std::filesystem::path &path) {
  std::ifstream in(path);
  require(bool(in), "failed to read fixture");
  return {std::istreambuf_iterator<char>(in), std::istreambuf_iterator<char>()};
}

std::filesystem::path write_temp(const std::string &name, std::string_view body) {
  const auto dir = std::filesystem::temp_directory_path() /
                   ("fairprice-test-" + std::to_string(::getpid()));
  std::filesystem::create_directories(dir);
  const auto path = dir / name;
  std::ofstream out(path);
  out << body;
  return path;
}

void test_example_and_shadow_config() {
  auto loaded = fairprice::load_config(FAIRPRICE_EXAMPLE_CONFIG);
  require(bool(loaded), loaded.message.c_str());
  require(loaded.value.version == 2 && loaded.value.port == 9093 &&
              loaded.value.shm_prefix == "/selfquant.mds" &&
              loaded.value.persistence.enabled &&
              loaded.value.market_data.sample_interval_ms == 200 &&
              loaded.value.market_data.depth == 50 &&
              loaded.value.market_data.clickhouse.service == "8443" &&
              loaded.value.market_data.clickhouse.catalog_scope == "prod-agg" &&
              loaded.value.market_data.clickhouse.catalog_table ==
                  "aggregate_topic_catalog" &&
              loaded.value.market_data.clickhouse.bbo_table ==
                  "aggregate_bbo_history" &&
              loaded.value.market_data.clickhouse.orderbook_table ==
                  "aggregate_orderbook_history" &&
              loaded.value.market_data.clickhouse.request_timeout_ms == 5000 &&
              loaded.value.market_data.clickhouse.password ==
                  "CHANGE_ME_CLICKHOUSE_PASSWORD" &&
              loaded.value.market_data.gateway_bootstrap.url ==
                  "ws://127.0.0.1:9443/v1/market-data" &&
              loaded.value.market_data.gateway_bootstrap.token ==
                  "CHANGE_ME_GATEWAY_TOKEN" &&
              loaded.value.persistence.database_url.find("postgres://") == 0 &&
              loaded.value.browser_token.empty(),
          "example yaml did not load production defaults");
  auto validated = fairprice::validate_loaded_config(loaded.value);
  require(bool(validated), validated.message.c_str());
  require(std::find(loaded.value.allowed_origins.begin(),
                    loaded.value.allowed_origins.end(),
                    "http://13.113.54.96:3000") !=
                  loaded.value.allowed_origins.end() &&
              std::find(loaded.value.allowed_origins.begin(),
                        loaded.value.allowed_origins.end(),
                        "http://127.0.0.1:3000") !=
                  loaded.value.allowed_origins.end(),
          "production origins are missing");
  auto shadow = fairprice::load_config(FAIRPRICE_SHADOW_CONFIG);
  require(bool(shadow), shadow.message.c_str());
  require(shadow.value.port == 19093 &&
              shadow.value.shm_prefix == "/selfquant.mds.shadow" &&
              !shadow.value.persistence.enabled &&
              shadow.value.persistence.database_url.empty() &&
              shadow.value.market_data.clickhouse.catalog_scope == "prod-agg",
          "shadow yaml is not isolated");
  auto shadow_ok = fairprice::validate_loaded_config(shadow.value);
  require(bool(shadow_ok), shadow_ok.message.c_str());
  require(std::find(shadow.value.allowed_origins.begin(),
                    shadow.value.allowed_origins.end(),
                    "http://localhost:3000") !=
                  shadow.value.allowed_origins.end() &&
              std::find(shadow.value.allowed_origins.begin(),
                        shadow.value.allowed_origins.end(),
                        "http://127.0.0.1:3000") !=
                  shadow.value.allowed_origins.end(),
          "shadow origins are missing");
}

std::string yaml_v2_body(std::string_view clickhouse_extra = {},
                         std::string_view gateway_url =
                             "ws://127.0.0.1:9443/v1/market-data",
                         std::string_view token = "test-token",
                         bool persistence = true,
                         std::string_view database_url =
                             "postgres://u:p@127.0.0.1:5432/db") {
  std::string yaml;
  yaml += "version: 2\nruntime:\n  log_level: info\n  development: true\n";
  yaml += "market_data:\n  sample_interval_ms: 200\n  depth: 50\n";
  yaml += "  snapshot_stale_after_ms: 15000\n  catalog_refresh_interval_ms: 1000\n";
  yaml += "  clickhouse:\n    host: 127.0.0.1\n    service: \"8443\"\n";
  yaml += "    database: market_data\n    user: bbo_writer\n";
  yaml += "    password: \"secret#pass\"\n";
  yaml += clickhouse_extra;
  yaml += "  gateway_bootstrap:\n    url: \"";
  yaml += gateway_url;
  yaml += "\"\n    token: \"";
  yaml += token;
  yaml += "\"\n    timeout_ms: 5000\n";
  yaml += "shared_memory:\n  backend: posix_shm\n  output:\n";
  yaml += "    prefix: /selfquant.mds\n    ring_bytes: 8388608\n";
  yaml += "    max_record_bytes: 65536\n    max_readers: 32\n";
  yaml += "    reader_lease_timeout_ms: 5000\n    unlink_on_shutdown: false\n";
  yaml += "server:\n  listen_address: 127.0.0.1\n  port: 9093\n";
  yaml += "  allowed_origins:\n    - http://127.0.0.1:3000\n";
  yaml += "  browser_token: \"\"\n  max_clients: 8\n";
  yaml += "  max_subscriptions_per_client: 4\n  read_header_timeout_ms: 5000\n";
  yaml += "  idle_timeout_ms: 60000\n  max_header_bytes: 16384\n";
  yaml += "  websocket:\n    handshake_timeout_ms: 5000\n    read_limit_bytes: 4096\n";
  yaml += "    write_buffer_bytes: 65536\n    pong_timeout_ms: 45000\n";
  yaml += "stream:\n  interval_ms: 50\nhistory:\n  workers: 1\n  resolution_ms: 60000\n";
  yaml += "fair_price:\n  enabled: true\n  depth: 20\n  lambda_per_bp: 0.1\n";
  yaml += "  imbalance_alpha: 1.0\n  ewma_tau_ms: 500\n  impact_notional: 0.0\n";
  yaml += "  venue_dominance_ratio: 0.9\n  stream_interval_ms: 5\n  heartbeat_ms: 1000\n";
  yaml += "  persistence:\n    enabled: ";
  yaml += persistence ? "true" : "false";
  yaml += "\n    database_url: \"";
  yaml += database_url;
  yaml += "\"\n    sample_interval_ms: 1000\n    retention_hours: 24\n";
  yaml += "    cleanup_interval_ms: 300000\n    delete_batch: 10000\n";
  yaml += "    delete_max_batches: 20\n    operation_timeout_ms: 5000\n";
  return yaml;
}

void test_yaml_v2_direct_secrets_and_defaults() {
  auto path = write_temp("v2.yaml", yaml_v2_body());
  auto loaded = fairprice::load_config(path.string());
  require(bool(loaded), loaded.message.c_str());
  require(loaded.value.version == 2, "version must be 2");
  require(loaded.value.market_data.clickhouse.password == "secret#pass",
          "password did not round-trip quoted yaml");
  require(loaded.value.market_data.gateway_bootstrap.url ==
              "ws://127.0.0.1:9443/v1/market-data",
          "gateway url did not round-trip");
  require(loaded.value.market_data.gateway_bootstrap.token == "test-token",
          "gateway token did not round-trip");
  require(loaded.value.persistence.database_url ==
              "postgres://u:p@127.0.0.1:5432/db",
          "database_url did not round-trip");
  require(loaded.value.browser_token.empty(), "empty browser_token must load");
  require(loaded.value.market_data.clickhouse.catalog_table ==
              "aggregate_topic_catalog",
          "catalog_table default");
  require(loaded.value.market_data.clickhouse.bbo_table ==
              "aggregate_bbo_history",
          "bbo_table default");
  require(loaded.value.market_data.clickhouse.orderbook_table ==
              "aggregate_orderbook_history",
          "orderbook_table default");
  require(loaded.value.market_data.clickhouse.catalog_scope == "prod-agg",
          "catalog_scope default");
  require(loaded.value.market_data.clickhouse.request_timeout_ms == 5000,
          "request_timeout_ms default");
  auto validated = fairprice::validate_loaded_config(loaded.value);
  require(bool(validated), validated.message.c_str());

  auto persist_empty =
      write_temp("persist-empty.yaml",
                 yaml_v2_body({}, "ws://127.0.0.1:9443/v1/market-data",
                             "test-token", true, ""));
  auto persist_loaded = fairprice::load_config(persist_empty.string());
  require(!persist_loaded, "enabled persistence with empty database_url");

  auto gw_empty = write_temp(
      "gw-empty.yaml",
      yaml_v2_body({}, "ws://127.0.0.1:9443/v1/market-data", ""));
  auto gw_loaded = fairprice::load_config(gw_empty.string());
  require(!gw_loaded, "empty gateway token must fail");

  auto gw_bad = write_temp("gw-bad.yaml", yaml_v2_body({}, "http://127.0.0.1",
                                                       "test-token"));
  auto gw_bad_loaded = fairprice::load_config(gw_bad.string());
  require(!gw_bad_loaded, "non-ws gateway url must fail");
}

void test_old_env_and_internal_keys_rejected() {
  const char *keys[] = {
      "    password_env: CLICKHOUSE_PASSWORD\n",
      "    catalog_table: aggregate_topic_catalog\n",
      "    bbo_table: aggregate_bbo_history\n",
      "    orderbook_table: aggregate_orderbook_history\n",
      "    catalog_scope: prod-agg\n",
      "    request_timeout_ms: 5000\n",
  };
  for (const char *extra : keys) {
    auto path = write_temp("old-ch.yaml", yaml_v2_body(extra));
    auto loaded = fairprice::load_config(path.string());
    require(!loaded, "internal or legacy clickhouse key was accepted");
  }
  auto url_env = write_temp(
      "url-env.yaml",
      "version: 2\nruntime:\n  log_level: info\n  development: true\n"
      "market_data:\n  sample_interval_ms: 200\n  depth: 50\n"
      "  snapshot_stale_after_ms: 15000\n  catalog_refresh_interval_ms: 1000\n"
      "  clickhouse:\n    host: 127.0.0.1\n    service: \"8443\"\n"
      "    database: market_data\n    user: bbo_writer\n    password: \"x\"\n"
      "  gateway_bootstrap:\n    url_env: AGGDATA_MDS_GATEWAY_URL\n"
      "    token: \"t\"\n    timeout_ms: 5000\n");
  auto loaded = fairprice::load_config(url_env.string());
  require(!loaded, "url_env must be rejected");
}

void test_unknown_and_missing_keys() {
  const auto example = read_file(FAIRPRICE_EXAMPLE_CONFIG);
  auto unknown = write_temp("unknown.yaml", example + "\nextra: 1\n");
  auto loaded = fairprice::load_config(unknown.string());
  require(!loaded, "unknown root key was accepted");
  auto missing = write_temp("missing.yaml", "version: 2\n");
  loaded = fairprice::load_config(missing.string());
  require(!loaded, "incomplete yaml was accepted");
  auto v1 = write_temp("v1.yaml", yaml_v2_body());
  auto body = read_file(v1);
  body.replace(body.find("version: 2"), 10, "version: 1");
  auto v1_path = write_temp("v1-only.yaml", body);
  loaded = fairprice::load_config(v1_path.string());
  require(!loaded, "version 1 must be rejected");
}

void test_model_id_default() {
  fairprice::FairPriceConfig config;
  std::string error;
  auto engine = fairprice::Engine::create(config, error);
  require(bool(engine), error.c_str());
  require(engine->model_id() == "fp-v1:8ab898513b4bd5ff",
          "default model_id does not match Go golden");
}

fairprice::BookView book_from_json(simdjson::dom::object object,
                                   std::vector<fairprice::ResidualLevel> &bids,
                                   std::vector<fairprice::ResidualLevel> &asks) {
  fairprice::BookView book{};
  std::uint64_t u64 = 0;
  std::uint64_t u8 = 0;
  if (!object["exchange_ts_ns"].get(u64)) {
    book.exchange_ts_ns = u64;
  }
  if (!object["price_scale"].get(u8)) {
    book.price_scale = static_cast<std::uint8_t>(u8);
  }
  if (!object["quantity_scale"].get(u8)) {
    book.quantity_scale = static_cast<std::uint8_t>(u8);
  }
  if (!object["member_count"].get(u8)) {
    book.member_count = static_cast<std::uint8_t>(u8);
  }
  if (!object["member_mask"].get(u64)) {
    book.member_mask = static_cast<std::uint32_t>(u64);
  }
  if (!object["active_mask"].get(u64)) {
    book.active_mask = static_cast<std::uint32_t>(u64);
  }
  auto load_side = [](simdjson::dom::array array,
                      std::vector<fairprice::ResidualLevel> &out) {
    for (auto item : array) {
      simdjson::dom::object level;
      if (item.get(level)) {
        continue;
      }
      fairprice::ResidualLevel value{};
      std::int64_t number = 0;
      if (!level["price"].get(number)) {
        value.price = number;
      }
      if (!level["quantity"].get(number)) {
        value.quantity = number;
      }
      simdjson::dom::array venues;
      if (!level["venue_quantity"].get(venues)) {
        int index = 0;
        for (auto qty : venues) {
          if (index >= fairprice::kMaxVenues) {
            break;
          }
          if (!qty.get(number)) {
            value.venue_quantity[static_cast<std::size_t>(index)] = number;
          }
          ++index;
        }
      }
      out.push_back(value);
    }
  };
  simdjson::dom::array array;
  if (!object["bids"].get(array)) {
    load_side(array, bids);
  }
  if (!object["asks"].get(array)) {
    load_side(array, asks);
  }
  book.bids = bids;
  book.asks = asks;
  return book;
}

void test_engine_goldens() {
  for (const auto &entry :
       std::filesystem::directory_iterator(FAIRPRICE_GOLDEN_DIR)) {
    const auto name = entry.path().filename().string();
    if (name == "api_ws_contract.json" || name == "snapshot_rest.json" ||
        name == "model_id_default.json") {
      continue;
    }
    const auto body = read_file(entry.path());
    simdjson::dom::parser parser;
    simdjson::dom::element doc;
    require(!parser.parse(body).get(doc), "golden json parse failed");
    simdjson::dom::object root;
    require(!doc.get(root), "golden root is not an object");
    simdjson::dom::object config_object;
    require(!root["config"].get(config_object), "golden config missing");
    fairprice::FairPriceConfig config;
    std::int64_t i64 = 0;
    double number = 0;
    require(!config_object["depth_k"].get(i64), "depth_k missing");
    config.depth_k = static_cast<int>(i64);
    require(!config_object["lambda_per_bp"].get(number), "lambda missing");
    config.lambda_per_bp = number;
    require(!config_object["imbalance_alpha"].get(number), "alpha missing");
    config.imbalance_alpha = number;
    require(!config_object["ewma_tau_ns"].get(i64), "tau missing");
    config.ewma_tau_ns = static_cast<std::uint64_t>(i64);
    require(!config_object["impact_notional"].get(number), "impact missing");
    config.impact_notional = number;
    require(!config_object["venue_dominance_ratio"].get(number),
            "dominance missing");
    config.venue_dominance_ratio = number;
    std::string error;
    auto engine = fairprice::Engine::create(config, error);
    require(bool(engine), error.c_str());
    simdjson::dom::object snapshot_object;
    require(!root["snapshot"].get(snapshot_object), "snapshot missing");
    fairprice::SnapshotView snapshot{};
    bool ready = false;
    if (!snapshot_object["ready"].get(ready)) {
      snapshot.ready = ready;
    }
    std::uint64_t u64 = 0;
    if (!snapshot_object["ring_epoch"].get(u64)) {
      snapshot.ring_epoch = u64;
    }
    if (!snapshot_object["ring_sequence"].get(u64)) {
      snapshot.ring_sequence = u64;
    }
    if (!snapshot_object["generation"].get(u64)) {
      snapshot.generation = u64;
    }
    if (!snapshot_object["wall_ns"].get(u64)) {
      snapshot.wall_ns = u64;
    }
    std::vector<fairprice::ResidualLevel> bids;
    std::vector<fairprice::ResidualLevel> asks;
    fairprice::BookView book{};
    simdjson::dom::object book_object;
    if (!snapshot_object["book"].get(book_object)) {
      book = book_from_json(book_object, bids, asks);
      snapshot.book = &book;
    }
    fairprice::ModelState previous_state{};
    const fairprice::ModelState *previous = nullptr;
    simdjson::dom::object previous_object;
    if (!root["previous"].get(previous_object)) {
      if (!previous_object["wall_ns"].get(u64)) {
        previous_state.wall_ns = u64;
      }
      if (!previous_object["price"].get(number)) {
        previous_state.price = number;
      }
      previous = &previous_state;
    }
    fairprice::ModelState next{};
    const auto pod = engine->compute(snapshot, previous, &next);
    simdjson::dom::object expect;
    require(!root["expect"].get(expect), "expect missing");
    std::string_view model_id;
    require(!expect["model_id"].get(model_id), "model_id missing");
    require(fairprice::cstr_view(pod.model_id) == model_id,
            (name + " model_id mismatch").c_str());
    bool expect_ready = false;
    require(!expect["ready"].get(expect_ready), "ready missing");
    require(bool(pod.ready) == expect_ready, (name + " ready mismatch").c_str());
    bool expect_crossed = false;
    if (!expect["crossed"].get(expect_crossed)) {
      require(bool(pod.crossed) == expect_crossed,
              (name + " crossed mismatch").c_str());
    }
    bool expect_degraded = false;
    if (!expect["degraded"].get(expect_degraded)) {
      require(bool(pod.degraded) == expect_degraded,
              (name + " degraded mismatch").c_str());
    }
    simdjson::dom::element reasons;
    if (!expect["degraded_reasons"].get(reasons)) {
      if (reasons.is_null()) {
        require(pod.degraded_mask == 0,
                (name + " degraded_reasons should be null").c_str());
      } else {
        simdjson::dom::array array;
        require(!reasons.get(array),
                (name + " degraded_reasons is not an array").c_str());
        std::string expected_names;
        bool first = true;
        for (auto item : array) {
          std::string_view name_view;
          require(!item.get(name_view),
                  (name + " degraded_reasons item is not a string").c_str());
          if (!first) {
            expected_names += ',';
          }
          first = false;
          expected_names += '"';
          expected_names += std::string(name_view);
          expected_names += '"';
        }
        std::string actual_names;
        fairprice::append_degraded_reasons(pod.degraded_mask, actual_names);
        require(actual_names == expected_names,
                (name + " degraded_reasons mismatch").c_str());
      }
    }
    if (expect_ready) {
      std::int64_t mantissa = 0;
      require(!expect["price_mantissa"].get(mantissa), "price mantissa missing");
      require(pod.price == mantissa, (name + " price mismatch").c_str());
    } else {
      std::string_view reason;
      if (!expect["reset_reason"].get(reason)) {
        require(fairprice::cstr_view(pod.reset_reason) == reason,
                (name + " reset_reason mismatch").c_str());
      }
    }
  }
}

void test_contract_json() {
  const auto body = read_file(std::filesystem::path(FAIRPRICE_GOLDEN_DIR) /
                              "api_ws_contract.json");
  require(fairprice::encode_status_json("ok").find("\"status\":\"ok\"") !=
              std::string::npos,
          "healthz json mismatch");
  require(fairprice::encode_ack_json(true, "subscribe", "agg_binance", "BTCUSDT",
                                     "fairprice", "")
                  .find("\"channel\":\"fairprice\"") != std::string::npos,
          "subscribe ack mismatch");
  require(fairprice::encode_reset_json("agg_binance", "BTCUSDT", "fairprice",
                                       "source_stale")
                  .find("\"op\":\"reset\"") != std::string::npos,
          "reset json mismatch");
  require(body.find("subscribe_ack") != std::string::npos,
          "api contract fixture missing");
}

void test_store_book_only_triggers_fair() {
  std::string error;
  auto engine = fairprice::Engine::create({}, error);
  require(bool(engine), error.c_str());
  fairprice::Store store(std::move(engine), 15'000'000'000ULL);
  fairprice::CatalogSnapshot catalog;
  catalog.markets.push_back({"agg", "BTCUSDT", "/bbo", "/book", true});
  store.reconcile(catalog);
  utils::md::wire::AggBboRecord bbo{};
  bbo.gated_bid.price = 99;
  bbo.gated_ask.price = 101;
  bbo.raw_bid.price = 99;
  bbo.raw_ask.price = 101;
  auto key = fairprice::market_key("agg", "BTCUSDT");
  require(store.apply_bbo_key(key, 1, 1, 1, 1, bbo, nullptr).empty(),
          "bbo apply failed");
  auto view = store.copy_view(key);
  require(view && view->bbo.ready && !view->fair.ready,
          "bbo path published fair price");
  fairprice::BookSnapshot book{};
  book.price_scale = 0;
  book.quantity_scale = 0;
  book.member_count = 2;
  book.member_mask = 3;
  book.active_mask = 3;
  book.bid_count = 2;
  book.ask_count = 2;
  book.bids[0] = {99, 10, {}};
  book.bids[1] = {98, 10, {}};
  book.asks[0] = {101, 10, {}};
  book.asks[1] = {102, 10, {}};
  fairprice::FairPricePod published{};
  require(store.apply_book_key(key, 1, 1, 1, 1, book, &published).empty(),
          "book apply failed");
  require(published.ready == 1, "book path did not compute fair price");
}

void test_catalog_clickhouse() {
  const auto bbo = "/selfquant.mds.agg.btcusdt.aggbbo.2";
  const auto book = "/selfquant.mds.agg.btcusdt.aggorderbook.2";
  const auto body =
      catalog_json_row("agg", "BTCUSDT", "aggbbo", bbo, 20) + "\n" +
      catalog_json_row("agg", "BTCUSDT", "aggorderbook", book, 20) + "\n";
  fairprice::MarketDataRepository repo(test_ch_config(), sql_body_query(body));
  auto catalog = fairprice::Catalog::open(&repo, test_catalog_settings());
  require(bool(catalog), catalog.message.c_str());
  require(catalog.value.snapshot() &&
              catalog.value.snapshot()->markets.size() == 1 &&
              catalog.value.snapshot()->markets[0].recording &&
              catalog.value.snapshot()->markets[0].bbo_segment == bbo &&
              catalog.value.snapshot()->markets[0].book_segment == book,
          "catalog did not pair the active market");
  require(repo.last_sql().find("LIMIT 1 BY segment") != std::string::npos &&
              repo.last_sql().find("service_instance DESC") != std::string::npos &&
              repo.last_sql().find("catalog_scope") != std::string::npos,
          "catalog SQL must use segment latest plus service_instance tie-break");

  const auto incomplete =
      catalog_json_row("agg", "ETHUSDT", "aggbbo",
                       "/selfquant.mds.agg.ethusdt.aggbbo.2", 3) +
      "\n";
  fairprice::MarketDataRepository incomplete_repo(
      test_ch_config(), sql_body_query(incomplete));
  auto skipped =
      fairprice::Catalog::open(&incomplete_repo, test_catalog_settings());
  require(bool(skipped) && skipped.value.snapshot() &&
              skipped.value.snapshot()->markets.empty(),
          "incomplete BBO-only market must not enter /v1/markets");

  const auto eth_bbo = "/selfquant.mds.agg.ethusdt.aggbbo.2";
  const auto eth_book = "/selfquant.mds.agg.ethusdt.aggorderbook.2";
  const auto sol_bbo = "/selfquant.mds.agg.solusdt.aggbbo.2";
  const auto sol_book = "/selfquant.mds.agg.solusdt.aggorderbook.2";
  const auto eth_body =
      catalog_json_row("agg", "ETHUSDT", "aggbbo", eth_bbo, 20) + "\n" +
      catalog_json_row("agg", "ETHUSDT", "aggorderbook", eth_book, 20) + "\n";
  const auto sol_body =
      catalog_json_row("agg", "SOLUSDT", "aggbbo", sol_bbo, 20) + "\n" +
      catalog_json_row("agg", "SOLUSDT", "aggorderbook", sol_book, 20) + "\n";
  const std::string outcomes[] = {body, "", "", "", eth_body, "", "", sol_body};
  std::size_t calls = 0;
  fairprice::MarketDataRepository last_good_repo(
      test_ch_config(), [&](std::string_view) {
        fairprice::MarketDataQueryResult result;
        const auto &outcome = outcomes[calls++];
        if (outcome.empty()) {
          result.error = fairprice::MarketDataError::Timeout;
          result.message = "timeout";
        } else {
          result.body = outcome;
        }
        return result;
      });
  auto lasting = fairprice::Catalog::open(&last_good_repo, test_catalog_settings());
  require(bool(lasting) && lasting.value.snapshot()->markets.size() == 1 &&
              lasting.value.snapshot()->markets[0].symbol == "BTCUSDT",
          "first catalog load must succeed");
  for (int failure = 0; failure < 3; ++failure) {
    auto refresh = lasting.value.refresh();
    require(bool(refresh) && !refresh.value &&
                lasting.value.snapshot()->markets.size() == 1 &&
                lasting.value.snapshot()->markets[0].symbol == "BTCUSDT",
            "refresh failure must retain last-good catalog without stopping");
  }
  auto recovered = lasting.value.refresh();
  require(bool(recovered) && recovered.value &&
              lasting.value.snapshot()->markets.size() == 1 &&
              lasting.value.snapshot()->markets[0].symbol == "ETHUSDT" &&
              lasting.value.snapshot()->markets[0].bbo_segment == eth_bbo,
          "successful refresh must replace last-good catalog");
  for (int failure = 0; failure < 2; ++failure) {
    auto refresh = lasting.value.refresh();
    require(bool(refresh) && !refresh.value &&
                lasting.value.snapshot()->markets[0].symbol == "ETHUSDT",
            "later failures must keep the replaced catalog");
  }
  auto restored = lasting.value.refresh();
  require(bool(restored) && restored.value &&
              lasting.value.snapshot()->markets[0].symbol == "SOLUSDT",
          "recovery after repeated failures must not stay failed");

  fairprice::MarketDataRepository cold(
      test_ch_config(), [](std::string_view) {
        return fairprice::MarketDataQueryResult{
            .error = fairprice::MarketDataError::Http, .message = "down"};
      });
  auto failed = fairprice::Catalog::open(&cold, test_catalog_settings());
  require(!failed, "cold start without catalog must fail");
}

void test_topic_id_learning() {
  const auto first = "/selfquant.mds.agg.btcusdt.aggbbo.2";
  const auto second = "/selfquant.mds.agg.ethusdt.aggbbo.2";
  fairprice::TopicIdLearner learner({second, first});
  require(learner.learning_segment().empty(), "learner starts idle");
  fairprice::GatewayWireFrame stray;
  stray.kind = fairprice::Kind::Bbo;
  stray.topic_id = 9;
  stray.bbo = test_bbo_record("BTC", "USDT", 99, 101);
  require(!learner.apply(stray), "unknown topic without learning must be dropped");
  require(!learner.segment_mapped(first), "stray frame must not map");

  learner.begin_learning(first);
  require(learner.apply(stray), "learning segment must bind unknown topic");
  require(learner.segment_mapped(first), "first segment must be mapped");
  require(learner.segment_for_topic(9) == first, "topic 9 must stay on first segment");

  learner.begin_learning(second);
  fairprice::GatewayWireFrame mapped = stray;
  mapped.ring_sequence = 2;
  require(learner.apply(mapped), "already mapped topic must keep processing");
  require(learner.segment_for_topic(9) == first,
          "mapped topic must not rebind during later learning");

  fairprice::GatewayWireFrame eth;
  eth.kind = fairprice::Kind::Bbo;
  eth.topic_id = 3;
  eth.bbo = test_bbo_record("ETH", "USDT", 9, 11);
  require(learner.apply(eth), "current learning segment must bind a new topic");
  require(learner.segment_mapped(second), "second segment must be mapped");

  fairprice::GatewayBootstrapClient client(
      {}, [&](std::string_view segment, std::uint64_t) {
        fairprice::GatewayWireFrame frame;
        frame.kind = fairprice::Kind::Bbo;
        if (segment.find("btcusdt") != std::string_view::npos) {
          frame.topic_id = 1;
          frame.bbo = test_bbo_record("BTC", "USDT", 99, 101);
        } else {
          frame.topic_id = 2;
          frame.bbo = test_bbo_record("ETH", "USDT", 9, 11);
        }
        frame.wall_ns = 5;
        return std::vector<fairprice::GatewayWireFrame>{frame};
      });
  const auto learned = client.run({second, first});
  require(learned.size() == 2, "bootstrap must learn sorted segments");
  require(learned[0].segment < learned[1].segment || learned.size() == 2,
          "snapshots are returned");

  int calls = 0;
  std::uint64_t max_wait = 0;
  fairprice::GatewayBootstrapClient timeout_client(
      fairprice::GatewayBootstrapConfig{.timeout_ms = 40},
      [&](std::string_view, std::uint64_t wait_ms) {
        ++calls;
        max_wait = std::max(max_wait, wait_ms);
        return std::vector<fairprice::GatewayWireFrame>{};
      });
  (void)timeout_client.run({first, second});
  require(calls == 2, "single-segment timeout must continue to the next");
  require(max_wait <= 40, "segment waits must stay within the total deadline");
}

void test_sequence_generation_bbo_and_book() {
  fairprice::Store store(std::nullopt, 15'000'000'000ULL);
  fairprice::CatalogSnapshot catalog;
  catalog.markets.push_back({"agg", "BTCUSDT", "/bbo", "/book", true});
  store.reconcile(catalog);
  const auto key = fairprice::market_key("agg", "BTCUSDT");
  utils::md::wire::AggBboRecord bbo{};
  bbo.member_count = 1;
  bbo.venue_slot_ids[0] = 1;
  bbo.gated_bid.price = 99;
  bbo.gated_ask.price = 101;
  bbo.raw_bid.price = 99;
  bbo.raw_ask.price = 101;
  std::memcpy(bbo.base_asset.data(), "BTC", 3);
  std::memcpy(bbo.quote_asset.data(), "USDT", 4);
  require(store.apply_bbo_key(key, 1, 1, 1, 1'000, bbo, nullptr).empty(),
          "bbo first apply failed");
  require(store.apply_bbo_key(key, 1, 2, 1, 2'000, bbo, nullptr).empty(),
          "bbo same-generation sequence increment reset ready");
  require(store.apply_bbo_key(key, 1, 2, 1, 2'000, bbo, nullptr).empty(),
          "bbo duplicate sequence should be ignored");
  auto view = store.copy_view(key);
  require(view && view->bbo.ready, "bbo lost ready after same generation");
  require(store.apply_bbo_key(key, 1, 3, 2, 2'500, bbo, nullptr).empty(),
          "bbo generation increase should be accepted");
  require(store.apply_bbo_key(key, 2, 1, 1, 2'600, bbo, nullptr).empty(),
          "bbo epoch change should be accepted");
  view = store.copy_view(key);
  require(view && view->bbo.ready, "bbo lost ready after epoch change");
  require(store.apply_bbo_key(key, 2, 2, 0, 3'000, bbo, nullptr) ==
              "generation rollback",
          "bbo generation rollback was not detected");
  store.reset_bbo(key, "generation_rollback", 1, 3, 0, nullptr);
  view = store.copy_view(key);
  require(view && !view->bbo.ready, "bbo stayed ready after generation rollback");

  fairprice::BookSnapshot book{};
  book.member_count = 1;
  book.venue_slot_ids[0] = 1;
  book.bid_count = 1;
  book.ask_count = 1;
  book.bids[0] = {99, 10, {}, 1, 1};
  book.asks[0] = {101, 10, {}, 1, 1};
  require(store.apply_book_key(key, 1, 1, 1, 1'000, book, nullptr).empty(),
          "book first apply failed");
  require(store.apply_book_key(key, 1, 2, 1, 2'000, book, nullptr).empty(),
          "book same-generation sequence increment reset ready");
  require(store.apply_book_key(key, 1, 2, 1, 2'000, book, nullptr).empty(),
          "book duplicate sequence should be ignored");
  view = store.copy_view(key);
  require(view && view->book.ready, "book lost ready after same generation");
  require(store.apply_book_key(key, 1, 3, 2, 2'500, book, nullptr).empty(),
          "book generation increase should be accepted");
  require(store.apply_book_key(key, 2, 1, 1, 2'600, book, nullptr).empty(),
          "book epoch change should be accepted");
  view = store.copy_view(key);
  require(view && view->book.ready, "book lost ready after epoch change");
  require(store.apply_book_key(key, 2, 2, 0, 3'000, book, nullptr) ==
              "generation rollback",
          "book generation rollback was not detected");
  store.reset_book(key, "generation_rollback", 1, 3, 0, nullptr);
  view = store.copy_view(key);
  require(view && !view->book.ready, "book stayed ready after generation rollback");
}

void test_readyz_freshness_and_markets_json() {
  fairprice::Store store(std::nullopt, 1'000'000'000ULL);
  fairprice::CatalogSnapshot catalog;
  catalog.markets.push_back({"agg", "BTCUSDT", "/bbo", "/book", true});
  store.reconcile(catalog);
  require(!store.ready(1'000), "catalog without snapshots was ready");
  const auto key = fairprice::market_key("agg", "BTCUSDT");
  utils::md::wire::AggBboRecord bbo{};
  bbo.member_count = 1;
  bbo.venue_slot_ids[0] = 1;
  bbo.gated_bid.price = 99;
  bbo.gated_ask.price = 101;
  bbo.raw_bid.price = 99;
  bbo.raw_ask.price = 101;
  std::memcpy(bbo.base_asset.data(), "BTC", 3);
  std::memcpy(bbo.quote_asset.data(), "USDT", 4);
  require(store.apply_bbo_key(key, 1, 1, 1, 1'000, bbo, nullptr).empty(),
          "bbo apply failed");
  require(!store.ready(1'000), "bbo-only market was ready");
  fairprice::BookSnapshot book{};
  book.member_count = 1;
  book.venue_slot_ids[0] = 1;
  book.bid_count = 1;
  book.ask_count = 1;
  book.bids[0] = {99, 10, {}, 1, 1};
  book.asks[0] = {101, 10, {}, 1, 1};
  std::memcpy(book.base_asset.data(), "BTC", 3);
  std::memcpy(book.quote_asset.data(), "USDT", 4);
  require(store.apply_book_key(key, 1, 1, 1, 5'000, book, nullptr).empty(),
          "book apply failed");
  require(store.ready(5'000), "fresh snapshots were not ready");
  require(!store.ready(2'000'000'000ULL), "stale snapshots stayed ready");
  store.reset_bbo(key, "reader_unavailable", 1, 1, 1, nullptr);
  require(!store.ready(5'000), "reader_unavailable did not clear readyz");
  require(store.apply_bbo_key(key, 1, 2, 1, 1'000, bbo, nullptr).empty(),
          "bbo recover apply failed");
  require(store.ready(5'000), "recovered snapshots were not ready");
  const auto markets = store.markets_json(5'000);
  require(markets.size() == 1 && markets[0].has_bbo && markets[0].has_order_book &&
              markets[0].recording && markets[0].live && markets[0].base == "BTC" &&
              markets[0].quote == "USDT" && markets[0].updated_ns == 1'000 &&
              markets[0].venues.size() == 1 && markets[0].venues[0] == "binance",
          "markets json identity did not follow BBO-first snapshot");
  const auto encoded = fairprice::encode_markets_json(markets);
  require(encoded.find("\"has_order_book\":true") != std::string::npos &&
              encoded.find("\"has_bbo\":true") != std::string::npos &&
              encoded.find("\"recording\":true") != std::string::npos &&
              encoded.find("\"live\":true") != std::string::npos &&
              encoded.find("\"updated_ns\":1000") != std::string::npos &&
              encoded.find("\"base\":\"BTC\"") != std::string::npos &&
              encoded.find("\"quote\":\"USDT\"") != std::string::npos &&
              encoded.find("\"venues\":[\"binance\"]") != std::string::npos,
          "markets json missing Go identity fields");
  require(fairprice::venue_name(99) == "venue-99",
          "unknown venue id did not match Go venue-%d format");
}

void require_sqab_header(const std::vector<std::byte> &frame, std::uint8_t kind) {
  require(frame.size() >= 42, "SQAB shorter than header");
  std::uint32_t magic = 0;
  std::memcpy(&magic, frame.data(), 4);
  require(magic == fairprice::kSqabMagic, "SQAB magic mismatch");
  require(frame[4] == std::byte{1} && frame[5] == std::byte{0} &&
              frame[6] == std::byte{kind} && frame[7] == std::byte{0},
          "SQAB version/kind mismatch");
  std::uint16_t header_bytes = 0;
  std::memcpy(&header_bytes, frame.data() + 8, 2);
  require(header_bytes == 42, "SQAB header bytes mismatch");
  std::uint32_t body_length = 0;
  std::memcpy(&body_length, frame.data() + 10, 4);
  require(body_length == static_cast<std::uint32_t>(frame.size() - 42),
          "SQAB body length mismatch");
}

void require_frontend_book_frame(const std::vector<std::byte> &frame) {
  require_sqab_header(frame, fairprice::kSqabKindBook);
  const auto symbol_len = static_cast<std::uint8_t>(frame[14]);
  const auto venue_count = static_cast<std::uint8_t>(frame[17]);
  std::size_t offset = 42;
  require(offset + symbol_len + venue_count + 4 <= frame.size(),
          "truncated SQAB metadata");
  offset += symbol_len + venue_count;
  std::uint16_t bids = 0;
  std::uint16_t asks = 0;
  std::memcpy(&bids, frame.data() + offset, 2);
  std::memcpy(&asks, frame.data() + offset + 2, 2);
  offset += 4;
  auto consume = [&](std::uint16_t count) {
    for (std::uint16_t i = 0; i < count; ++i) {
      require(offset + 21 <= frame.size(), "truncated SQAB level");
      std::uint32_t mask = 0;
      std::memcpy(&mask, frame.data() + offset + 16, 4);
      const auto contributors = static_cast<std::uint8_t>(frame[offset + 20]);
      offset += 21;
      std::uint32_t seen = 0;
      for (std::uint8_t c = 0; c < contributors; ++c) {
        require(offset + 9 <= frame.size(), "truncated SQAB contribution");
        const auto slot = static_cast<std::uint8_t>(frame[offset]);
        require(slot < venue_count && (mask & (1U << slot)) != 0,
                "invalid SQAB venue slot");
        seen |= 1U << slot;
        offset += 9;
      }
      require(seen == mask, "SQAB venue mask mismatch");
    }
  };
  consume(bids);
  consume(asks);
  require(offset == frame.size(), "SQAB frame has trailing bytes");
}

void test_sqab_cache_and_snapshot_fields() {
  fairprice::Store store(std::nullopt, 15'000'000'000ULL);
  fairprice::CatalogSnapshot catalog;
  catalog.markets.push_back({"agg", "BTCUSDT", "/bbo", "/book", true});
  store.reconcile(catalog);
  const auto key = fairprice::market_key("agg", "BTCUSDT");
  const auto before = store.sqab_encode_count();
  utils::md::wire::AggBboRecord bbo{};
  bbo.member_count = 1;
  bbo.venue_slot_ids[0] = 1;
  bbo.gated_bid.price = 99;
  bbo.gated_bid.quantity = 10;
  bbo.gated_bid.venue_mask = 1;
  bbo.gated_bid.contributor_count = 1;
  bbo.gated_bid.venue_quantity[0] = 10;
  bbo.gated_ask.price = 101;
  bbo.gated_ask.quantity = 10;
  bbo.gated_ask.venue_mask = 1;
  bbo.gated_ask.contributor_count = 1;
  bbo.gated_ask.venue_quantity[0] = 10;
  bbo.raw_bid.price = 99;
  bbo.raw_bid.quantity = 10;
  bbo.raw_bid.venue_mask = 1;
  bbo.raw_ask.price = 101;
  bbo.raw_ask.quantity = 10;
  bbo.raw_ask.venue_mask = 1;
  require(store.apply_bbo_key(key, 1, 1, 1, 1, bbo, nullptr).empty(),
          "bbo apply failed");
  require(store.sqab_encode_count() == before + 1,
          "bbo encoded more than one SQAB frame");
  require(store.apply_bbo_key(key, 1, 1, 1, 1, bbo, nullptr).empty(),
          "duplicate bbo should not encode");
  require(store.sqab_encode_count() == before + 1,
          "duplicate bbo increased encode count");
  fairprice::BookSnapshot book{};
  book.member_count = 1;
  book.venue_slot_ids[0] = 1;
  book.bid_count = 21;
  book.ask_count = 21;
  for (int i = 0; i < 21; ++i) {
    fairprice::ResidualLevel level{};
    level.price = 99 - i;
    level.quantity = 10;
    level.venue_quantity[0] = 10;
    level.venue_mask = 1;
    level.contributor_count = 1;
    book.bids[static_cast<std::size_t>(i)] = level;
    level.price = 101 + i;
    book.asks[static_cast<std::size_t>(i)] = level;
  }
  require(store.apply_book_key(key, 1, 1, 1, 1, book, nullptr).empty(),
          "book apply failed");
  require(store.sqab_encode_count() == before + 3,
          "orderbook did not encode depth 20 and 50 once");
  const auto after_clients = store.sqab_encode_count();
  require(after_clients == before + 3,
          "SQAB encode count changed without a new apply");
  auto view = store.copy_view(key);
  require(view && view->bbo.browser && view->bbo.browser->size() >= 42,
          "bbo SQAB cache missing");
  require(view && view->book.browser20 && view->book.browser50 &&
              view->book.browser20->size() != view->book.browser50->size(),
          "orderbook SQAB 20/50 caches missing");
  require_sqab_header(*view->bbo.browser, fairprice::kSqabKindBbo);
  require_frontend_book_frame(*view->book.browser20);
  require_frontend_book_frame(*view->book.browser50);
  const auto json = fairprice::encode_snapshot_json(
      "agg", "BTCUSDT", &view->bbo, &view->book, 20);
  require(json.find("\"raw_spread_bps\"") != std::string::npos &&
              json.find("\"gated_spread_bps\"") != std::string::npos &&
              json.find("\"raw_cross_bps\"") != std::string::npos &&
              json.find("\"gated_cross_bps\"") != std::string::npos &&
              json.find("\"sequence\":\"") != std::string::npos,
          "snapshot json missing spread/cross fields");
  const auto depth1 = fairprice::encode_snapshot_json(
      "agg", "BTCUSDT", &view->bbo, &view->book, 1);
  require(depth1.find("\"bids\":[") != std::string::npos,
          "depth=1 snapshot missing bids");
  fairprice::HistoryResponse history{};
  history.symbol = "BTCUSDT";
  history.type = "gated";
  const auto history_json = fairprice::encode_history_json(history);
  require(history_json.find("\"start_ns\"") != std::string::npos &&
              history_json.find("\"buckets\"") != std::string::npos &&
              history_json.find("\"gaps\"") != std::string::npos &&
              history_json.find("\"distribution_bps\"") != std::string::npos,
          "spread history json missing Go fields");
}

void test_reader_fault_policy() {
  require(fairprice::reader_fault_policy(mds::api::ErrorCode::RecordOverwritten) ==
              fairprice::ReaderFaultPolicy::ResyncInPlace,
          "RecordOverwritten must resync in place");
  require(fairprice::reader_fault_policy(
              mds::api::ErrorCode::SubscriptionRejected) ==
              fairprice::ReaderFaultPolicy::ResyncInPlace,
          "SubscriptionRejected must resync in place");
  require(fairprice::reader_fault_policy(mds::api::ErrorCode::InvalidHandle) ==
              fairprice::ReaderFaultPolicy::DestroyAndBackoff,
          "InvalidHandle must destroy and backoff");
  require(fairprice::reader_fault_policy(mds::api::ErrorCode::Ok) ==
              fairprice::ReaderFaultPolicy::Ignore,
          "other reader errors must not force reopen");
}

fairprice::LiveSample make_sample(std::uint64_t wall_ns, double raw,
                                  double gated) {
  fairprice::LiveSample sample{};
  sample.wall_ns = wall_ns;
  sample.raw_bps = raw;
  sample.gated_bps = gated;
  sample.raw_min = raw;
  sample.raw_max = raw;
  sample.gated_min = gated;
  sample.gated_max = gated;
  sample.window_samples = 1;
  return sample;
}

void test_live_sample_layout_unchanged() {
  require(sizeof(fairprice::LiveSample) == 64, "LiveSample size changed");
  require(offsetof(fairprice::LiveSample, wall_ns) == 0,
          "LiveSample.wall_ns offset changed");
  require(offsetof(fairprice::LiveSample, raw_bps) == 8,
          "LiveSample.raw_bps offset changed");
  require(offsetof(fairprice::LiveSample, gated_bps) == 16,
          "LiveSample.gated_bps offset changed");
  require(offsetof(fairprice::LiveSample, window_samples) == 56,
          "LiveSample.window_samples offset changed");
}

void test_distribution_downsample() {
  require(fairprice::downsample_distribution({}).empty(),
          "empty distribution must stay empty");
  std::vector<double> small{1.5, 2.5, 3.5};
  require(fairprice::downsample_distribution(small) == small,
          "n<10000 distribution must stay unchanged");
  std::vector<double> source;
  source.reserve(20'001);
  for (int i = 0; i <= 20'000; ++i) {
    source.push_back(static_cast<double>(i));
  }
  const auto sampled = fairprice::downsample_distribution(source);
  require(sampled.size() == fairprice::kMaximumDistributionSamples,
          "distribution must cap at 10000");
  require(sampled.front() == 0.0 && sampled.back() == 20'000.0,
          "downsample must keep first and last");
  for (std::size_t i = 1; i < sampled.size(); ++i) {
    require(sampled[i] >= sampled[i - 1], "downsampled distribution must stay ordered");
  }
}

void test_live_overlay_and_aggregate() {
  const auto shard_same = make_sample(100, 1.0, 10.0);
  const auto live_same = make_sample(100, 9.0, 90.0);
  const auto shard_later = make_sample(200, 2.0, 20.0);
  const auto merged = fairprice::merge_history_samples(
      {shard_same, shard_later}, {live_same});
  require(merged.size() == 2, "equal wall_ns samples must dedupe to one");
  require(merged[0].raw_bps == 9.0 && merged[0].gated_bps == 90.0,
          "live must overlay shard even when live is not last in input");
  require(merged[1].raw_bps == 2.0, "distinct wall_ns samples must remain");

  const auto now = 3'600'000'000'000ULL;
  const auto in_window = make_sample(now - 1'000'000'000ULL, 4.0, 40.0);
  const auto duplicate = make_sample(now - 1'000'000'000ULL, 5.0, 50.0);
  const auto deduped = fairprice::merge_history_samples({in_window}, {duplicate});
  auto raw = fairprice::aggregate_history("BTCUSDT", "raw", now, 60'000'000'000ULL,
                                          200, deduped);
  auto gated = fairprice::aggregate_history("BTCUSDT", "gated", now,
                                            60'000'000'000ULL, 200, deduped);
  require(raw.distribution.size() == 1 && raw.distribution[0] == 5.0,
          "raw aggregation must use live overlay");
  require(gated.distribution.size() == 1 && gated.distribution[0] == 50.0,
          "gated aggregation must use live overlay");
  require(!raw.buckets.empty() && raw.buckets.front().samples == 1,
          "bucket sample count must be after dedupe");
  require(raw.symbol == "BTCUSDT" && raw.type == "raw",
          "aggregate_history must preserve identity fields");

  const auto day_ns = 24ULL * 3600ULL * 1'000'000'000ULL;
  const auto res = 60'000'000'000ULL;
  const auto window_end = day_ns + 8 * res;
  const auto window_start = window_end - day_ns;
  const auto bucket = window_start + 3 * res;
  const auto cut = bucket + 30'000'000'000ULL;
  fairprice::HistoryBucket ch_bucket{};
  ch_bucket.start_ns = bucket;
  ch_bucket.open = 10;
  ch_bucket.high = 20;
  ch_bucket.low = 10;
  ch_bucket.close = 20;
  ch_bucket.window_min = 1;
  ch_bucket.window_max = 2;
  ch_bucket.samples = 2;
  fairprice::HistoryResponse stored;
  stored.symbol = "BTCUSDT";
  stored.type = "gated";
  stored.start_ns = window_start;
  stored.end_ns = window_end;
  stored.resolution_ns = res;
  stored.buckets = {ch_bucket};
  stored.distribution = {10.0, 20.0, 10.0};
  const auto dist_before = stored.distribution;
  const auto live = std::vector<fairprice::LiveSample>{
      make_sample(cut, 30.0, 30.0),
      make_sample(bucket + 10'000'000'000ULL, 999.0, 999.0),
      make_sample(cut + 10'000'000'000ULL, 40.0, 40.0),
  };
  fairprice::overlay_ohlc_live(stored, live, cut, 1000);
  require(stored.distribution == dist_before,
          "live overlay must not change CCDF");
  const fairprice::HistoryBucket *merged_bucket = nullptr;
  for (const auto &item : stored.buckets) {
    if (item.start_ns == bucket) {
      merged_bucket = &item;
      break;
    }
  }
  require(merged_bucket != nullptr, "cut overlay must keep the straddle bucket");
  require(merged_bucket->samples == 4,
          "cut overlay samples must be CH-before plus live-after, not overlapped");
  require(merged_bucket->open == 10.0 && merged_bucket->close == 40.0 &&
              merged_bucket->high == 40.0 && merged_bucket->low == 10.0,
          "cut overlay OHLC must splice earlier open and later close");
  require(merged_bucket->window_min == 1.0 && merged_bucket->window_max == 40.0,
          "cut overlay window must keep CH extrema plus live extrema");
  require(merged_bucket->coverage <= 4.0 / 60.0 + 1e-12,
          "coverage must not exceed unique-sample expectation");
}

std::string ohlc_row_json(std::uint64_t start_ns, double open, double high,
                          double low, double close, double window_min,
                          double window_max, std::uint64_t samples) {
  return "{\"bucket_start_ns\":\"" + std::to_string(start_ns) +
         "\",\"open\":" + std::to_string(open) +
         ",\"high\":" + std::to_string(high) + ",\"low\":" + std::to_string(low) +
         ",\"close\":" + std::to_string(close) +
         ",\"window_min\":" + std::to_string(window_min) +
         ",\"window_max\":" + std::to_string(window_max) + ",\"samples\":\"" +
         std::to_string(samples) + "\"}\n";
}

void test_clickhouse_history_and_partial_window() {
  const auto segment = "/selfquant.mds.agg.btcusdt.aggbbo.2";
  const auto now = 3'600'000'000'000ULL;
  const auto res = 60'000'000'000ULL;
  const auto start_ns = 0;
  std::string ohlc;
  std::string distribution;
  std::vector<fairprice::LiveSample> fixture;
  for (int minute = 10; minute >= 1; --minute) {
    const auto sampled =
        now - static_cast<std::uint64_t>(minute) * 60ULL * 1'000'000'000ULL;
    const auto bucket = start_ns + ((sampled - start_ns) / res) * res;
    const auto bps = fairprice::signed_spread_bps(99, 101);
    ohlc += ohlc_row_json(bucket, bps, bps, bps, bps, 1, 2, 1);
    distribution += "{\"bps\":" + std::to_string(bps) + "}\n";
    auto sample = make_sample(sampled, bps, bps);
    sample.raw_min = 1;
    sample.raw_max = 2;
    sample.gated_min = 1;
    sample.gated_max = 2;
    fixture.push_back(sample);
  }
  fairprice::MarketDataRepository repo(
      test_ch_config(),
      sql_body_query("", "", "", "", ohlc, distribution));
  auto sql_check = repo.query_bbo_history_ohlc(segment, start_ns, now, res, "gated");
  require(bool(sql_check), "ohlc fixture must parse");
  require(repo.last_sql().find("GROUP BY bucket_start_ns") != std::string::npos &&
              repo.last_sql().find("intDiv") != std::string::npos &&
              repo.last_sql().find("raw_cross_bps") == std::string::npos &&
              repo.last_sql().find("argMin") != std::string::npos,
          "OHLC SQL must aggregate signed spread buckets without raw_cross_bps");
  fairprice::History provider(&repo, res);
  fairprice::CatalogMarket market;
  market.symbol = "BTCUSDT";
  market.bbo_segment = segment;
  auto response = provider.query(market, 200, {}, "gated", now);
  require(!response.gaps.empty(), "partial history must report gaps");
  require(response.distribution.size() == 10,
          "CCDF must use the 10-minute sample set");
  auto expected = fairprice::aggregate_history("BTCUSDT", "gated", now, res, 200,
                                               fixture);
  require(!response.buckets.empty() && !expected.buckets.empty(),
          "fixture OHLC and aggregate_history must both produce buckets");
  require(response.buckets.front().samples == expected.buckets.front().samples &&
              response.buckets.front().open == expected.buckets.front().open &&
              response.buckets.front().window_min ==
                  expected.buckets.front().window_min,
          "CH OHLC fixture must match aggregate_history OHLC/count/window");
  require(repo.last_sql().find("arrayJoin") != std::string::npos &&
              repo.last_sql().find("raw_cross_bps") == std::string::npos,
          "CCDF SQL must downsample in ClickHouse without raw_cross_bps");

  const auto day_ns = 24ULL * 3600ULL * 1'000'000'000ULL;
  const auto window_end = day_ns + 8 * res;
  const auto window_start = window_end - day_ns;
  const auto bucket = window_start + 3 * res;
  const auto cut = bucket + 30'000'000'000ULL;
  const auto ch_ohlc = ohlc_row_json(bucket, 10, 20, 10, 20, 1, 2, 2);
  const auto ch_dist = std::string("{\"bps\":10}\n{\"bps\":20}\n");
  fairprice::MarketDataRepository cut_repo(
      test_ch_config(),
      sql_body_query("", "", "", "", ch_ohlc, ch_dist));
  fairprice::History cut_provider(&cut_repo, res);
  std::vector<fairprice::LiveSample> live{
      make_sample(cut, 30.0, 30.0),
      make_sample(bucket + 10'000'000'000ULL, 999.0, 999.0),
      make_sample(cut + 10'000'000'000ULL, 40.0, 40.0),
  };
  auto cut_response =
      cut_provider.query(market, 1000, live, "gated", window_end);
  require(cut_response.distribution.size() == 2 &&
              cut_response.distribution.front() == 10.0 &&
              cut_response.distribution.back() == 20.0,
          "CCDF must stay on ClickHouse downsample and ignore live overlay");
  const fairprice::HistoryBucket *cut_bucket = nullptr;
  for (const auto &item : cut_response.buckets) {
    if (item.start_ns == bucket) {
      cut_bucket = &item;
      break;
    }
  }
  require(cut_bucket && cut_bucket->samples == 4 && cut_bucket->open == 10.0 &&
              cut_bucket->close == 40.0 && cut_bucket->high == 40.0 &&
              cut_bucket->low == 10.0,
          "History::query cut overlay must splice without double-counting");
  require(cut_bucket->coverage <= 4.0 / 60.0 + 1e-12,
          "History::query coverage must not exceed unique samples");

  int calls = 0;
  fairprice::MarketDataRepository fail_repo(
      test_ch_config(), [&](std::string_view sql) {
        fairprice::MarketDataQueryResult result;
        ++calls;
        const auto text = std::string(sql);
        if (text.find("GROUP BY bucket_start_ns") != std::string::npos) {
          result.error = fairprice::MarketDataError::Http;
          result.message = "ohlc failed";
          return result;
        }
        result.body = "{\"bps\":1}\n";
        return result;
      });
  fairprice::History fail_provider(&fail_repo, res);
  auto failed = fail_provider.query(market, 200, {}, "gated", now);
  require(!failed.unavailable_shards.empty() &&
              failed.unavailable_shards.front() == segment,
          "OHLC failure must fill unavailable_shards");
  require(failed.distribution.size() == 1,
          "distribution can still return when OHLC fails");
}

void test_owned_orderbook_latest_wins() {
  fairprice::OwnedOrderBookFrame latest{};
  utils::md::wire::AggOrderBookRecord first{};
  first.bid_count = 1;
  utils::md::wire::AggOrderBookRecord second{};
  second.bid_count = 2;
  utils::md::wire::AggOrderBookRecord third{};
  third.bid_count = 3;
  fairprice::consider_latest_orderbook(latest, first, 1, 10, 1, 100);
  fairprice::consider_latest_orderbook(latest, second, 1, 12, 1, 300);
  fairprice::consider_latest_orderbook(latest, third, 1, 11, 1, 200);
  require(latest.present && latest.sequence == 12 && latest.record.bid_count == 2 &&
              latest.frames == 3 && latest.wall_ns == 300,
          "owned orderbook candidate must keep the highest sequence");
}

void test_last_good_without_reset() {
  std::string error;
  auto engine = fairprice::Engine::create({}, error);
  require(bool(engine), error.c_str());
  fairprice::Store store(std::move(engine), 15'000'000'000ULL);
  fairprice::CatalogSnapshot catalog;
  catalog.markets.push_back({"agg", "BTCUSDT", "/bbo", "/book", true});
  store.reconcile(catalog);
  auto key = fairprice::market_key("agg", "BTCUSDT");
  fairprice::BookSnapshot book{};
  book.member_count = 2;
  book.member_mask = 3;
  book.active_mask = 3;
  book.bid_count = 2;
  book.ask_count = 2;
  book.bids[0] = {99, 10, {}};
  book.bids[1] = {98, 10, {}};
  book.asks[0] = {101, 10, {}};
  book.asks[1] = {102, 10, {}};
  require(store.apply_book_key(key, 1, 1, 1, 1'000, book, nullptr).empty(),
          "seed book apply failed");
  auto view = store.copy_view(key);
  require(view && view->book.ready && view->fair.ready,
          "seed book must be ready");
  require(store.apply_book_key(key, 1, 2, 0, 2'000, book, nullptr) ==
              "generation rollback",
          "generation rollback must be detected");
  view = store.copy_view(key);
  require(view && view->book.ready && view->fair.ready,
          "generation rollback without reset must keep last-good");
  require(!store.mark_stale(key, 1'000 + 1'000'000'000ULL, nullptr),
          "last-good must stay fresh before the stale threshold");
  view = store.copy_view(key);
  require(view && view->book.ready && view->fair.ready,
          "unexpired last-good must remain ready");
  require(store.mark_stale(key, 1'000 + 16'000'000'000ULL, nullptr),
          "expired last-good must become source_stale");
  view = store.copy_view(key);
  require(view && !view->book.ready &&
              fairprice::cstr_view(view->fair.reset_reason) == "source_stale",
          "stale threshold must still clear last-good");
}

void test_sample_tick_and_history_retry() {
  const auto interval = 1'000'000'000ULL;
  auto next_tick = interval;
  std::uint64_t now = 0;
  int samples = 0;
  std::uint64_t missed_total = 0;
  while (samples < 300) {
    if (now < next_tick) {
      now = next_tick;
    }
    ++samples;
    auto plan = fairprice::next_sample_tick(now, interval, next_tick + interval);
    missed_total += plan.missed;
    next_tick = plan.sleep_until_mono_ns;
  }
  require(samples == 300 && missed_total == 0 && next_tick == 301 * interval,
          "instant 1s ticks over 300s must sample 300 times without catch-up");

  next_tick = interval;
  now = interval + 3 * interval;
  auto late = fairprice::next_sample_tick(now, interval, next_tick + interval);
  require(late.missed >= 1 && late.sleep_until_mono_ns > now,
          "a slow sample must skip missed ticks instead of bursting writes");

  require(fairprice::should_retry_history(503), "503 history must be retryable");
  require(!fairprice::should_retry_history(400) &&
              !fairprice::should_retry_history(200),
          "400/200 history must not retry");
  std::string error;
  require(fairprice::validate_fair_price_history("nope", "2020-01-01T01:00:00Z",
                                                 "1s", error) == 400,
          "invalid fair-price-history window must be 400");
  require(fairprice::validate_fair_price_history(
              "2020-01-01T00:00:00Z", "2020-01-01T01:00:00Z", "1s", error) == 0,
          "valid fair-price-history window must pass");
}

std::string history_row_with_bid(std::string_view bid_json) {
  return std::string("{\"sampled_at_ns\":\"1\",\"raw_bid_price\":") +
         std::string(bid_json) +
         ",\"raw_ask_price\":101,\"gated_bid_price\":99,\"gated_ask_price\":101,"
         "\"cross_window_raw_min\":1,\"cross_window_raw_max\":2,"
         "\"cross_window_gated_min\":1,\"cross_window_gated_max\":2,"
         "\"cross_window_samples\":\"1\"}\n";
}

fairprice::MarketDataResult<std::vector<fairprice::HistoryBboRow>>
parse_history_bid(std::string_view bid_json) {
  fairprice::MarketDataRepository repo(
      test_ch_config(),
      sql_body_query("", "", "", history_row_with_bid(bid_json)));
  return repo.query_bbo_history("seg", 0, 10);
}

void test_element_i64_number_and_string() {
  auto ok = parse_history_bid("99");
  require(bool(ok) && ok.value.size() == 1 && ok.value[0].raw_bid_price == 99,
          "signed JSON number must parse");
  auto negative = parse_history_bid("-123");
  require(bool(negative) && negative.value[0].raw_bid_price == -123,
          "negative JSON number must parse");
  auto unsigned_max = parse_history_bid("9223372036854775807");
  require(bool(unsigned_max) &&
              unsigned_max.value[0].raw_bid_price == 9223372036854775807LL,
          "unsigned JSON number at INT64 max must parse");
  auto unsigned_overflow = parse_history_bid("9223372036854775808");
  require(!unsigned_overflow, "unsigned JSON number above INT64 max must fail");
  auto quoted = parse_history_bid("\"8486137\"");
  require(bool(quoted) && quoted.value[0].raw_bid_price == 8486137,
          "ClickHouse quoted Int64 must parse");
  auto quoted_neg = parse_history_bid("\"-123\"");
  require(bool(quoted_neg) && quoted_neg.value[0].raw_bid_price == -123,
          "quoted negative Int64 must parse");
  auto quoted_min = parse_history_bid("\"-9223372036854775808\"");
  require(bool(quoted_min) &&
              quoted_min.value[0].raw_bid_price ==
                  (-9223372036854775807LL - 1),
          "quoted INT64_MIN decimal must parse");
  auto quoted_max = parse_history_bid("\"9223372036854775807\"");
  require(bool(quoted_max) &&
              quoted_max.value[0].raw_bid_price == 9223372036854775807LL,
          "quoted INT64_MAX decimal must parse");
  require(!parse_history_bid("\"\""), "empty string must fail");
  require(!parse_history_bid("\"1x\""), "trailing junk must fail");
  require(!parse_history_bid("\"1.2\""), "decimal string must fail");
  require(!parse_history_bid("\"9223372036854775808\""),
          "quoted overflow must fail");
  require(!parse_history_bid("\"INT64_MIN\""),
          "INT64_MIN token must not parse");
}

std::string json_num(bool quote, std::string_view value) {
  return quote ? (std::string("\"") + std::string(value) + "\"")
               : std::string(value);
}

std::string i64_array8(bool quote, std::string_view first) {
  std::string out = "[";
  out += json_num(quote, first);
  for (int i = 0; i < 7; ++i) {
    out += ",";
    out += json_num(quote, "0");
  }
  out += "]";
  return out;
}

std::string latest_bbo_json(bool quote_i64) {
  const std::string seg =
      "/selfquant.mds.agg_spot_usdt_binance-bitget-bybit-gate-okx.btcusdt.aggbbo.2";
  std::string json = "{";
  json += "\"sampled_at_ns\":" + json_num(quote_i64, "1000") + ",";
  json += "\"source_wall_ns\":" + json_num(quote_i64, "2000") + ",";
  json += "\"segment\":\"" + seg + "\",";
  json += "\"ring_epoch\":" + json_num(quote_i64, "1") + ",";
  json += "\"ring_sequence\":" + json_num(quote_i64, "2") + ",";
  json += "\"generation\":" + json_num(quote_i64, "3") + ",";
  json += "\"header_flags\":0,\"header_exchange_ts_ns\":" +
          json_num(quote_i64, "4") + ",";
  json += "\"base_asset\":\"BTC\",\"quote_asset\":\"USDT\",";
  json += "\"venue_slot_ids\":[1,0,0,0,0,0,0,0],";
  json += "\"price_scale\":2,\"quantity_scale\":3,\"member_count\":1,";
  json += "\"member_mask\":1,\"live_mask\":1,";
  json += "\"gated_bid_price\":" + json_num(quote_i64, "8486137") + ",";
  json += "\"gated_bid_quantity\":" + json_num(quote_i64, "10") + ",";
  json += "\"gated_bid_venue_quantity\":" + i64_array8(quote_i64, "10") + ",";
  json += "\"gated_bid_exchange_ts_ns\":" + json_num(quote_i64, "5") + ",";
  json += "\"gated_bid_venue_mask\":1,\"gated_bid_worst_ingress_age_us\":0,";
  json += "\"gated_bid_best_venue\":0,\"gated_bid_timestamp_venue\":0,";
  json += "\"gated_bid_contributor_count\":1,";
  json += "\"gated_ask_price\":" + json_num(quote_i64, "8484660") + ",";
  json += "\"gated_ask_quantity\":" + json_num(quote_i64, "11") + ",";
  json += "\"gated_ask_venue_quantity\":" + i64_array8(quote_i64, "11") + ",";
  json += "\"gated_ask_exchange_ts_ns\":" + json_num(quote_i64, "6") + ",";
  json += "\"gated_ask_venue_mask\":1,\"gated_ask_worst_ingress_age_us\":0,";
  json += "\"gated_ask_best_venue\":0,\"gated_ask_timestamp_venue\":0,";
  json += "\"gated_ask_contributor_count\":1,";
  json += "\"raw_bid_price\":" + json_num(quote_i64, "8486137") + ",";
  json += "\"raw_bid_quantity\":" + json_num(quote_i64, "10") + ",";
  json += "\"raw_bid_venue_mask\":1,\"raw_bid_best_venue\":0,";
  json += "\"raw_ask_price\":" + json_num(quote_i64, "8484660") + ",";
  json += "\"raw_ask_quantity\":" + json_num(quote_i64, "11") + ",";
  json += "\"raw_ask_venue_mask\":1,\"raw_ask_best_venue\":0,";
  json += "\"raw_cross_bps\":1,\"gated_cross_bps\":2,\"skew_us\":0,";
  json += "\"cross_skew_threshold_us\":0,\"fx_age_us\":0,";
  json += "\"cross_bid_venue\":0,\"cross_ask_venue\":0,\"fx_venue\":0}";
  json += "\n";
  return json;
}

std::string latest_book_json() {
  const std::string seg =
      "/selfquant.mds.agg_spot_usdt_binance-bitget-bybit-gate-okx.btcusdt."
      "aggorderbook.2";
  std::string json = "{";
  json += "\"sampled_at_ns\":\"1000\",\"source_wall_ns\":\"2000\",";
  json += "\"segment\":\"" + seg + "\",";
  json += "\"ring_epoch\":\"1\",\"ring_sequence\":\"2\",\"generation\":\"3\",";
  json += "\"header_flags\":0,\"exchange_ts_ns\":\"4\",\"book_generation\":1,";
  json += "\"base_asset\":\"BTC\",\"quote_asset\":\"USDT\",";
  json += "\"venue_slot_ids\":[1,0,0,0,0,0,0,0],";
  json += "\"price_scale\":2,\"quantity_scale\":3,\"member_count\":1,";
  json += "\"member_mask\":1,\"active_mask\":1,\"bid_count\":1,\"ask_count\":1,";
  json += "\"bid_price\":[\"8486137\"],\"bid_quantity\":[\"10\"],";
  json += "\"bid_venue_mask\":[1],\"bid_contributor_count\":[1],";
  json += "\"bid_venue_quantity\":[" + i64_array8(true, "10") + "],";
  json += "\"ask_price\":[\"8484660\"],\"ask_quantity\":[\"11\"],";
  json += "\"ask_venue_mask\":[1],\"ask_contributor_count\":[1],";
  json += "\"ask_venue_quantity\":[" + i64_array8(true, "11") + "]}";
  json += "\n";
  return json;
}

void test_latest_bbo_quoted_and_number() {
  const std::string seg =
      "/selfquant.mds.agg_spot_usdt_binance-bitget-bybit-gate-okx.btcusdt.aggbbo.2";
  {
    fairprice::MarketDataRepository repo(
        test_ch_config(), sql_body_query("", latest_bbo_json(true)));
    auto rows = repo.query_latest_bbo({seg});
    require(bool(rows), rows.message.c_str());
    require(rows.value.size() == 1, "quoted latest BBO must return one row");
    require(rows.value[0].record.gated_bid.price == 8486137 &&
                rows.value[0].record.gated_ask.price == 8484660 &&
                rows.value[0].record.gated_bid.quantity == 10 &&
                rows.value[0].record.gated_bid.venue_quantity[0] == 10,
            "quoted latest BBO fields mismatch");
  }
  {
    fairprice::MarketDataRepository repo(
        test_ch_config(), sql_body_query("", latest_bbo_json(false)));
    auto rows = repo.query_latest_bbo({seg});
    require(bool(rows), rows.message.c_str());
    require(rows.value.size() == 1 &&
                rows.value[0].record.raw_bid.price == 8486137 &&
                rows.value[0].record.raw_ask.price == 8484660,
            "JSON number latest BBO must parse");
  }
}

void test_latest_book_quoted_arrays() {
  const std::string seg =
      "/selfquant.mds.agg_spot_usdt_binance-bitget-bybit-gate-okx.btcusdt."
      "aggorderbook.2";
  fairprice::MarketDataRepository repo(
      test_ch_config(), sql_body_query("", "", latest_book_json()));
  auto rows = repo.query_latest_book({seg});
  require(bool(rows), rows.message.c_str());
  require(rows.value.size() == 1, "quoted latest book must return one row");
  require(rows.value[0].record.bid_count == 1 &&
              rows.value[0].record.ask_count == 1 &&
              rows.value[0].record.bids[0].price == 8486137 &&
              rows.value[0].record.bids[0].quantity == 10 &&
              rows.value[0].record.bids[0].venue_quantity[0] == 10 &&
              rows.value[0].record.asks[0].price == 8484660 &&
              rows.value[0].record.asks[0].quantity == 11 &&
              rows.value[0].record.asks[0].venue_quantity[0] == 11,
          "quoted latest book arrays mismatch");
}

void test_latest_fallback_is_batched() {
  int calls = 0;
  fairprice::MarketDataRepository repo(
      test_ch_config(), [&](std::string_view sql) {
        ++calls;
        fairprice::MarketDataQueryResult result;
        require(std::string(sql).find("LIMIT 1 BY segment") != std::string::npos &&
                    std::string(sql).find("IN (") != std::string::npos,
                "latest fallback must use batched IN + LIMIT 1 BY segment");
        return result;
      });
  const auto bbo = repo.query_latest_bbo(
      {"/selfquant.mds.agg.btcusdt.aggbbo.2",
       "/selfquant.mds.agg.ethusdt.aggbbo.2",
       "/selfquant.mds.agg.solusdt.aggbbo.2"});
  require(static_cast<bool>(bbo), "empty batched BBO latest is success");
  require(calls == 1, "BBO latest must be one HTTP query for all segments");
  const auto book = repo.query_latest_book(
      {"/selfquant.mds.agg.btcusdt.aggorderbook.2",
       "/selfquant.mds.agg.ethusdt.aggorderbook.2",
       "/selfquant.mds.agg.solusdt.aggorderbook.2"});
  require(static_cast<bool>(book), "empty batched OrderBook latest is success");
  require(calls == 2, "OrderBook latest must be a second batched HTTP query");
  require(repo.query_count() == 2,
          "missing sides must not query ClickHouse once per topic");
}

void test_restore_clock_and_if_empty() {
  fairprice::Store store(std::nullopt, 1'000'000'000ULL);
  fairprice::CatalogSnapshot catalog;
  catalog.markets.push_back({"agg", "BTCUSDT", "/bbo", "/book", true});
  store.reconcile(catalog);
  const auto key = fairprice::market_key("agg", "BTCUSDT");
  const auto sampled = 5'000'000'000ULL;
  const auto source_wall = 1ULL;
  auto bbo = test_bbo_record("BTC", "USDT", 99, 101);
  auto sampled_restore =
      store.restore_bbo_if_empty(key, 7, 3, 2, sampled, bbo, sampled + 1);
  require(sampled_restore.restored && sampled_restore.fresh,
          "CH restore must use sampled_at_ns as wall_ns");
  auto view = store.copy_view(key);
  require(view && view->bbo.wall_ns == sampled,
          "store wall_ns must be sampled_at_ns");
  require(store.fresh(sampled, sampled + 1), "sampled_at clock must be fresh");
  require(!store.fresh(source_wall, sampled + 1),
          "source_wall_ns as wall_ns would be stale and must not be used");

  auto again =
      store.restore_bbo_if_empty(key, 8, 4, 3, sampled + 10, bbo, sampled + 10);
  require(!again.restored, "restore_if_empty must skip existing identity");

  fairprice::BookSnapshot live_book{};
  live_book.member_count = 1;
  live_book.venue_slot_ids[0] = 1;
  live_book.bid_count = 1;
  live_book.ask_count = 1;
  live_book.bids[0] = {199, 10, {}, 1, 1};
  live_book.asks[0] = {201, 10, {}, 1, 1};
  std::memcpy(live_book.base_asset.data(), "BTC", 3);
  std::memcpy(live_book.quote_asset.data(), "USDT", 4);
  require(store.apply_book_key(key, 9, 1, 1, sampled, live_book, nullptr).empty(),
          "live book apply failed");
  auto skip_live = store.restore_book_if_empty(
      key, 1, 9, 9, 1,
      fairprice::book_snapshot_from_record(test_book_record("SOL", "USDT", 9, 11)),
      sampled);
  require(!skip_live.restored, "live identity must skip restore");
}

void test_degraded_reasons_helpers() {
  require(fairprice::postgres_text_array_from_mask(0) == "{}",
          "mask=0 must produce empty postgres text[]");

  const struct {
    std::uint32_t bit;
    const char *expected;
  } bits[] = {
      {fairprice::kDegradedBookCrossed, R"({"book_crossed"})"},
      {fairprice::kDegradedInsufficientDepth, R"({"insufficient_depth"})"},
      {fairprice::kDegradedImpactDepth, R"({"impact_depth_insufficient"})"},
      {fairprice::kDegradedInactiveVenues, R"({"inactive_venues"})"},
      {fairprice::kDegradedVenueDominant, R"({"single_venue_dominant"})"},
  };
  for (const auto &row : bits) {
    require(fairprice::postgres_text_array_from_mask(row.bit) == row.expected,
            "single degraded bit must map to the live JSON name");
    require(fairprice::postgres_text_array_from_mask(row.bit) != "{}",
            "nonzero mask must not produce an empty postgres array");
  }

  const auto combined = fairprice::kDegradedBookCrossed |
                        fairprice::kDegradedInactiveVenues;
  std::string live_names;
  fairprice::append_degraded_reasons(combined, live_names);
  require(fairprice::postgres_text_array_from_mask(combined) ==
              "{" + live_names + "}",
          "combined bits must keep live JSON name order");
  require(fairprice::postgres_text_array_from_mask(combined) ==
              R"({"book_crossed","inactive_venues"})",
          "combined bits must emit all names");

  const auto all_bits = fairprice::kDegradedBookCrossed |
                        fairprice::kDegradedInsufficientDepth |
                        fairprice::kDegradedImpactDepth |
                        fairprice::kDegradedInactiveVenues |
                        fairprice::kDegradedVenueDominant;
  require(fairprice::postgres_text_array_from_mask(all_bits) ==
              R"({"book_crossed","insufficient_depth","impact_depth_insufficient","inactive_venues","single_venue_dominant"})",
          "all degraded bits must emit every name in live JSON order");

  const auto json_array =
      fairprice::history_degraded_reasons_json_field(R"(["book_crossed"])");
  require(json_array == R"(["book_crossed"])",
          "new history points must emit a legal JSON array");
  require(json_array.front() == '[' && json_array.back() == ']',
          "history reasons must be a JSON array");
  require(json_array != "[]",
          "degraded=true with a nonzero mask must not emit an empty array");

  const char *null_inputs[] = {"", "[]", "{}", "{book_crossed}", "book_crossed"};
  for (const auto *input : null_inputs) {
    const auto field = fairprice::history_degraded_reasons_json_field(input);
    require(field == "null",
            "empty, legacy postgres arrays, and non-array text must be null");
    require(field.find("{}") == std::string::npos,
            "history JSON must not emit postgres empty-array form");
    require(field.find("{book_crossed}") == std::string::npos,
            "history JSON must not emit postgres text-array form");
  }
}

fairprice::FairPriceConfig test_depth_config(int depth_k = 20) {
  fairprice::FairPriceConfig config;
  config.depth_k = depth_k;
  config.lambda_per_bp = 0.1;
  config.imbalance_alpha = 1;
  config.ewma_tau_ns = 500'000'000;
  config.impact_notional = 0;
  config.venue_dominance_ratio = 0.9;
  return config;
}

fairprice::ResidualLevel test_level(std::int64_t price, std::int64_t quantity,
                                    std::int64_t venue0, std::int64_t venue1) {
  fairprice::ResidualLevel level{};
  level.price = price;
  level.quantity = quantity;
  level.venue_quantity[0] = venue0;
  level.venue_quantity[1] = venue1;
  return level;
}

void fill_ladder(std::vector<fairprice::ResidualLevel> &side, int count,
                 std::int64_t start, std::int64_t step, std::int64_t quantity) {
  side.clear();
  side.reserve(static_cast<std::size_t>(count));
  const auto venue0 = quantity / 2;
  const auto venue1 = quantity - venue0;
  for (int i = 0; i < count; ++i) {
    side.push_back(test_level(start + i * step, quantity, venue0, venue1));
  }
}

fairprice::FairPricePod compute_book(
    const fairprice::FairPriceConfig &config,
    const std::vector<fairprice::ResidualLevel> &bids,
    const std::vector<fairprice::ResidualLevel> &asks,
    fairprice::ComputeStats *stats = nullptr, std::uint32_t active_mask = 3,
    std::uint32_t member_mask = 3) {
  std::string error;
  auto engine = fairprice::Engine::create(config, error);
  require(bool(engine), error.c_str());
  fairprice::BookView book{};
  book.price_scale = 0;
  book.quantity_scale = 0;
  book.member_count = 2;
  book.member_mask = member_mask;
  book.active_mask = active_mask;
  book.bids = bids;
  book.asks = asks;
  fairprice::SnapshotView snapshot{};
  snapshot.ready = true;
  snapshot.book = &book;
  fairprice::ModelState next{};
  return engine->compute(snapshot, nullptr, &next, stats);
}

void test_depth_not_absolute_degraded() {
  std::vector<fairprice::ResidualLevel> bids50;
  std::vector<fairprice::ResidualLevel> asks50;
  fill_ladder(bids50, 50, 100, -1, 10);
  fill_ladder(asks50, 50, 101, 1, 10);
  fairprice::ComputeStats stats50{};
  const auto full = compute_book(test_depth_config(20), bids50, asks50, &stats50);
  require(full.ready == 1, "50/50 must compute");
  require((full.degraded_mask & fairprice::kDegradedInsufficientDepth) == 0,
          "50/50 must not set insufficient_depth");
  require(stats50.actual_bid_depth == 50 && stats50.actual_ask_depth == 50,
          "50/50 actual depth is post-copy remaining");
  require(!stats50.target_depth_shortfall, "50/50 has no target shortfall");
  std::vector<fairprice::ResidualLevel> bids20(bids50.begin(),
                                               bids50.begin() + 20);
  std::vector<fairprice::ResidualLevel> asks20(asks50.begin(),
                                               asks50.begin() + 20);
  const auto truncated = compute_book(test_depth_config(20), bids20, asks20);
  require(full.price == truncated.price && full.price_raw == truncated.price_raw &&
              full.mid == truncated.mid && full.microprice == truncated.microprice,
          "50/50 must use only the first 20 levels");

  std::vector<fairprice::ResidualLevel> bids15;
  std::vector<fairprice::ResidualLevel> asks18;
  fill_ladder(bids15, 15, 100, -1, 10);
  fill_ladder(asks18, 18, 101, 1, 10);
  fairprice::ComputeStats stats15{};
  const auto short_book =
      compute_book(test_depth_config(20), bids15, asks18, &stats15);
  require(short_book.ready == 1, "15/18 must still compute");
  require((short_book.degraded_mask & fairprice::kDegradedInsufficientDepth) == 0,
          "15/18 must not set insufficient_depth");
  require(short_book.degraded == 0, "15/18 must not degrade only for shortfall");
  require(stats15.actual_bid_depth == 15 && stats15.actual_ask_depth == 18,
          "15/18 must use actual remaining levels");
  require(stats15.target_depth_shortfall, "15/18 is a target shortfall");
  auto old_mask = short_book;
  old_mask.degraded_mask |= fairprice::kDegradedInsufficientDepth;
  old_mask.degraded = 1;
  require(short_book.price == old_mask.price &&
              short_book.price_raw == old_mask.price_raw &&
              short_book.mid == old_mask.mid &&
              short_book.microprice == old_mask.microprice,
          "short-book prices must be invariant; only degraded flags may change");

  std::vector<fairprice::ResidualLevel> bid1{test_level(99, 10, 10, 0)};
  std::vector<fairprice::ResidualLevel> ask1{test_level(101, 10, 10, 0)};
  const auto one =
      compute_book(test_depth_config(20), bid1, ask1, nullptr, 1, 1);
  require(one.ready == 1, "1/1 must still compute");
  require((one.degraded_mask & fairprice::kDegradedInsufficientDepth) == 0,
          "1/1 must not set insufficient_depth");
  require((one.degraded_mask & fairprice::kDegradedVenueDominant) != 0,
          "1/1 single venue must set single_venue_dominant");

  std::vector<fairprice::ResidualLevel> empty_asks;
  const auto empty_side =
      compute_book(test_depth_config(20), bids15, empty_asks);
  require(empty_side.ready == 0, "empty ask side must not be ready");
  require(fairprice::cstr_view(empty_side.reset_reason) == "empty_side",
          "empty ask side must be empty_side");
  const auto empty_bid =
      compute_book(test_depth_config(20), empty_asks, asks18);
  require(empty_bid.ready == 0 &&
              fairprice::cstr_view(empty_bid.reset_reason) == "empty_side",
          "empty bid side must be empty_side");

  std::vector<fairprice::ResidualLevel> crossed_bids{
      test_level(101, 2, 1, 1), test_level(99, 3, 1, 2)};
  std::vector<fairprice::ResidualLevel> crossed_asks{
      test_level(100, 1, 0, 1), test_level(102, 4, 2, 2)};
  fairprice::ComputeStats uncross_stats{};
  const auto uncrossed =
      compute_book(test_depth_config(20), crossed_bids, crossed_asks,
                   &uncross_stats);
  require(uncrossed.ready == 1, "uncross remainder must compute");
  require(uncrossed.crossed == 1, "uncross remainder stays crossed");
  require((uncrossed.degraded_mask & fairprice::kDegradedInsufficientDepth) == 0,
          "uncross remainder must not set insufficient_depth");
  require(uncross_stats.actual_bid_depth > 0 && uncross_stats.actual_ask_depth > 0,
          "uncross remainder uses leftover levels");
  require(uncross_stats.target_depth_shortfall,
          "uncross remainder below depth_k is a shortfall");

  std::vector<fairprice::ResidualLevel> exhaust_bids{test_level(101, 1, 1, 0)};
  std::vector<fairprice::ResidualLevel> exhaust_asks{test_level(100, 1, 0, 1)};
  const auto exhausted =
      compute_book(test_depth_config(20), exhaust_bids, exhaust_asks);
  require(exhausted.ready == 0, "uncross exhausting a side is not ready");
  require(fairprice::cstr_view(exhausted.reset_reason) == "uncross_depth_exhausted",
          "uncross exhausting a side must be uncross_depth_exhausted");
}

void test_log_level_config() {
  const auto replace_level = [](std::string_view level) {
    auto body = yaml_v2_body();
    const auto pos = body.find("log_level: info");
    require(pos != std::string::npos, "yaml fixture missing log_level");
    body.replace(pos, std::strlen("log_level: info"),
                 std::string("log_level: ") + std::string(level));
    return body;
  };
  for (const char *level : {"info", "debug", "warn", "error"}) {
    auto path = write_temp(std::string("log-") + level + ".yaml",
                           replace_level(level));
    auto loaded = fairprice::load_config(path.string());
    require(bool(loaded), loaded.message.c_str());
    require(loaded.value.log_level == level, "log_level did not round-trip");
    auto validated = fairprice::validate_loaded_config(loaded.value);
    require(bool(validated), validated.message.c_str());
  }
  auto unknown = write_temp("log-unknown.yaml", replace_level("verbose"));
  auto unknown_loaded = fairprice::load_config(unknown.string());
  require(!unknown_loaded, "unknown log_level must fail startup");
  auto upper = write_temp("log-upper.yaml", replace_level("INFO"));
  auto upper_loaded = fairprice::load_config(upper.string());
  require(!upper_loaded, "uppercase log_level must fail startup");
}

void test_log_level_gate() {
  std::vector<std::string> lines;
  fairprice::set_log_capture_for_test(&lines);
  fairprice::init_log(fairprice::LogLevel::Info, "test-info");
  fairprice::log_debug("hidden debug");
  fairprice::log_info("visible info");
  require(lines.size() == 1, "info level must drop debug");
  require(lines[0].find("hidden debug") == std::string::npos,
          "development-style debug must not leak at info");
  require(lines[0].find("visible info") != std::string::npos,
          "info line missing");
  require(lines[0].find("instance=test-info") != std::string::npos,
          "instance id missing");
  require(lines[0].find("level=info") != std::string::npos, "level missing");
  require(lines[0].find("T") != std::string::npos &&
              lines[0].find("Z ") != std::string::npos,
          "timestamp prefix missing");
  lines.clear();
  fairprice::init_log(fairprice::LogLevel::Debug, "test-debug");
  fairprice::log_debug("shown debug");
  require(lines.size() == 1 && lines[0].find("shown debug") != std::string::npos,
          "debug level must emit debug");
  fairprice::set_log_capture_for_test(nullptr);
  fairprice::init_log(fairprice::LogLevel::Info, "test-reset");
}

void test_orderbook_log_summary_clock() {
  fairprice::OrderbookLogSummary summary;
  fairprice::ComputeStats stats{};
  stats.recorded = true;
  stats.actual_bid_depth = 15;
  stats.actual_ask_depth = 18;
  stats.target_depth_shortfall = true;
  summary.note_apply("agg:BTCUSDT", "agg", "BTCUSDT", 3, &stats);
  require(summary.poll(0).empty(), "first poll only arms the window");
  require(summary.poll(fairprice::kOrderbookLogSummaryPeriodNs - 1).empty(),
          "non-empty window must wait 10 minutes");
  auto first = summary.poll(fairprice::kOrderbookLogSummaryPeriodNs);
  require(first.size() == 1, "10 minutes must emit one orderbook summary");
  require(first[0].symbol == "BTCUSDT" && first[0].counters.frames == 3 &&
              first[0].counters.coalesced_drains == 1 &&
              first[0].counters.coalesced_extra_frames == 2 &&
              first[0].counters.target_depth_shortfall_count == 1 &&
              first[0].counters.actual_bid_depth == 15 &&
              first[0].counters.actual_ask_depth == 18,
          "summary counters mismatch");
  summary.note_apply("agg:BTCUSDT", "agg", "BTCUSDT", 1, &stats);
  require(summary.poll(fairprice::kOrderbookLogSummaryPeriodNs + 1).empty(),
          "second fire inside next window must not emit");
  auto second = summary.poll(fairprice::kOrderbookLogSummaryPeriodNs * 2);
  require(second.size() == 1, "next 10-minute window emits once");
  require(summary.poll(fairprice::kOrderbookLogSummaryPeriodNs * 3).empty(),
          "quiet window must stay silent");

  fairprice::OrderbookLogSummary partial;
  partial.note_apply("agg:ETHUSDT", "agg", "ETHUSDT", 2, &stats);
  require(partial.poll(10).empty(), "partial window arms without emit");
  auto flushed = partial.flush_on_stop();
  require(flushed.size() == 1 && flushed[0].symbol == "ETHUSDT",
          "stop must flush nonempty partial window");
  require(partial.flush_on_stop().empty(), "second flush must be empty");

  fairprice::OrderbookLogSummary quiet;
  require(quiet.poll(0).empty(), "quiet first poll");
  require(quiet.poll(fairprice::kOrderbookLogSummaryPeriodNs).empty(),
          "quiet market must not emit");
  require(quiet.flush_on_stop().empty(), "quiet stop flush must be empty");
}

void run_fairprice_unit_tests() {
  test_degraded_reasons_helpers();
  test_log_level_config();
  test_log_level_gate();
  test_orderbook_log_summary_clock();
  test_depth_not_absolute_degraded();
  test_example_and_shadow_config();
  test_yaml_v2_direct_secrets_and_defaults();
  test_old_env_and_internal_keys_rejected();
  test_unknown_and_missing_keys();
  test_model_id_default();
  test_engine_goldens();
  test_contract_json();
  test_store_book_only_triggers_fair();
  test_catalog_clickhouse();
  test_topic_id_learning();
  test_sequence_generation_bbo_and_book();
  test_readyz_freshness_and_markets_json();
  test_sqab_cache_and_snapshot_fields();
  test_reader_fault_policy();
  test_owned_orderbook_latest_wins();
  test_last_good_without_reset();
  test_live_sample_layout_unchanged();
  test_distribution_downsample();
  test_live_overlay_and_aggregate();
  test_clickhouse_history_and_partial_window();
  test_sample_tick_and_history_retry();
  test_element_i64_number_and_string();
  test_latest_bbo_quoted_and_number();
  test_latest_book_quoted_arrays();
  test_latest_fallback_is_batched();
  test_restore_clock_and_if_empty();
}

}  // namespace

void run_fairprice_service_tests();

int main() {
  try {
    run_fairprice_unit_tests();
    run_fairprice_service_tests();
  } catch (const std::exception &ex) {
    std::fprintf(stderr, "fairprice tests failed: %s\n", ex.what());
    return 1;
  }
  return 0;
}

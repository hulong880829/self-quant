#include "fairprice/market_data_repository.h"

#include "net/http_client.h"

#include <algorithm>
#include <cctype>
#include <charconv>
#include <chrono>
#include <cstdint>
#include <cstring>
#include <limits>
#include <poll.h>
#include <span>
#include <sstream>
#include <sys/epoll.h>
#include <system_error>

#include <simdjson.h>

namespace fairprice {
namespace {

bool identifier(std::string_view value) {
  return !value.empty() &&
         std::all_of(value.begin(), value.end(), [](unsigned char c) {
           return std::isalnum(c) != 0 || c == '_';
         });
}

bool element_i64(simdjson::dom::element field, std::int64_t &out) {
  std::int64_t signed_value = 0;
  if (!field.get(signed_value)) {
    out = signed_value;
    return true;
  }
  std::uint64_t unsigned_value = 0;
  if (!field.get(unsigned_value)) {
    if (unsigned_value > static_cast<std::uint64_t>(INT64_MAX)) {
      return false;
    }
    out = static_cast<std::int64_t>(unsigned_value);
    return true;
  }
  std::string_view text;
  if (field.get(text) || text.empty()) {
    return false;
  }
  std::int64_t parsed = 0;
  const auto *begin = text.data();
  const auto *end = begin + text.size();
  const auto result = std::from_chars(begin, end, parsed);
  if (result.ec != std::errc{} || result.ptr != end) {
    return false;
  }
  out = parsed;
  return true;
}

std::string url_encode(std::string_view value) {
  static constexpr char hex[] = "0123456789ABCDEF";
  std::string result;
  for (const char raw : value) {
    const auto c = static_cast<unsigned char>(raw);
    if (std::isalnum(c) != 0 || c == '-' || c == '_' || c == '.' || c == '~') {
      result.push_back(static_cast<char>(c));
    } else {
      result.push_back('%');
      result.push_back(hex[c >> 4U]);
      result.push_back(hex[c & 15U]);
    }
  }
  return result;
}

std::string sql_string(std::string_view value) {
  std::string out;
  out.push_back('\'');
  for (const char c : value) {
    if (c == '\'') {
      out += "''";
    } else {
      out.push_back(c);
    }
  }
  out.push_back('\'');
  return out;
}

std::string sql_in_list(const std::vector<std::string> &values) {
  std::string out = "(";
  for (std::size_t i = 0; i < values.size(); ++i) {
    if (i != 0) {
      out += ',';
    }
    out += sql_string(values[i]);
  }
  out += ')';
  return out;
}

std::string qualified(std::string_view database, std::string_view table) {
  return std::string(database) + '.' + std::string(table);
}

std::string signed_spread_sql(std::string_view bps_type) {
  const bool raw = bps_type == "raw";
  const char *bid = raw ? "raw_bid_price" : "gated_bid_price";
  const char *ask = raw ? "raw_ask_price" : "gated_ask_price";
  return std::string("if((") + bid + " <= 0) OR (" + ask + " <= 0), 0., (" +
         ask + " - " + bid + ") * 20000. / (" + ask + " + " + bid + "))";
}

std::string window_min_sql(std::string_view bps_type) {
  return bps_type == "raw" ? "min(cross_window_raw_min)"
                           : "min(cross_window_gated_min)";
}

std::string window_max_sql(std::string_view bps_type) {
  return bps_type == "raw" ? "max(cross_window_raw_max)"
                           : "max(cross_window_gated_max)";
}

MarketDataError classify_status(unsigned status) {
  if (status == 401 || status == 403) {
    return MarketDataError::Auth;
  }
  return MarketDataError::Http;
}

bool get_u64(simdjson::dom::object object, std::string_view key,
             std::uint64_t &out) {
  simdjson::dom::element field;
  if (object[key].get(field)) {
    return false;
  }
  std::uint64_t unsigned_value = 0;
  if (!field.get(unsigned_value)) {
    out = unsigned_value;
    return true;
  }
  std::int64_t signed_value = 0;
  if (!field.get(signed_value) && signed_value >= 0) {
    out = static_cast<std::uint64_t>(signed_value);
    return true;
  }
  std::string_view text;
  if (!field.get(text)) {
    try {
      out = static_cast<std::uint64_t>(std::stoull(std::string(text)));
      return true;
    } catch (...) {
      return false;
    }
  }
  return false;
}

bool get_i64(simdjson::dom::object object, std::string_view key,
             std::int64_t &out) {
  simdjson::dom::element field;
  if (object[key].get(field)) {
    return false;
  }
  return element_i64(field, out);
}

bool get_i32(simdjson::dom::object object, std::string_view key,
             std::int32_t &out) {
  std::int64_t value = 0;
  if (!get_i64(object, key, value) || value < INT32_MIN || value > INT32_MAX) {
    return false;
  }
  out = static_cast<std::int32_t>(value);
  return true;
}

bool get_f64(simdjson::dom::object object, std::string_view key, double &out) {
  simdjson::dom::element field;
  if (object[key].get(field)) {
    return false;
  }
  double number = 0;
  if (!field.get(number)) {
    out = number;
    return true;
  }
  std::int64_t signed_value = 0;
  if (element_i64(field, signed_value)) {
    out = static_cast<double>(signed_value);
    return true;
  }
  std::string_view text;
  if (field.get(text) || text.empty()) {
    return false;
  }
  try {
    out = std::stod(std::string(text));
    return true;
  } catch (...) {
    return false;
  }
}

bool get_u16(simdjson::dom::object object, std::string_view key,
             std::uint16_t &out) {
  std::uint64_t value = 0;
  if (!get_u64(object, key, value) || value > 0xffffU) {
    return false;
  }
  out = static_cast<std::uint16_t>(value);
  return true;
}

bool get_u32(simdjson::dom::object object, std::string_view key,
             std::uint32_t &out) {
  std::uint64_t value = 0;
  if (!get_u64(object, key, value) || value > 0xffffffffULL) {
    return false;
  }
  out = static_cast<std::uint32_t>(value);
  return true;
}

bool get_u8(simdjson::dom::object object, std::string_view key,
            std::uint8_t &out) {
  std::uint64_t value = 0;
  if (!get_u64(object, key, value) || value > 0xffU) {
    return false;
  }
  out = static_cast<std::uint8_t>(value);
  return true;
}

bool get_bool_u8(simdjson::dom::object object, std::string_view key,
                 bool &out) {
  std::uint64_t value = 0;
  if (get_u64(object, key, value)) {
    out = value != 0;
    return true;
  }
  bool flag = false;
  if (!object[key].get(flag)) {
    out = flag;
    return true;
  }
  return false;
}

bool get_string(simdjson::dom::object object, std::string_view key,
                std::string &out) {
  std::string_view text;
  if (object[key].get(text)) {
    return false;
  }
  out.assign(text.begin(), text.end());
  return true;
}

template <std::size_t N>
void copy_fixed(std::array<char, N> &dst, std::string_view src) {
  dst = {};
  std::memcpy(dst.data(), src.data(), std::min(src.size(), N));
}

bool fill_i64_array(simdjson::dom::element field, std::span<std::int64_t> out,
                    std::size_t expected, MarketDataError &error) {
  simdjson::dom::array values;
  if (field.get(values)) {
    error = MarketDataError::Parse;
    return false;
  }
  std::size_t count = 0;
  for (auto item : values) {
    if (count >= out.size()) {
      error = MarketDataError::ArrayLength;
      return false;
    }
    std::int64_t value = 0;
    if (!element_i64(item, value)) {
      error = MarketDataError::Parse;
      return false;
    }
    out[count++] = value;
  }
  if (expected != 0 && count != expected) {
    error = MarketDataError::ArrayLength;
    return false;
  }
  return true;
}

bool fill_u32_array(simdjson::dom::element field, std::span<std::uint32_t> out,
                    std::size_t expected, MarketDataError &error) {
  simdjson::dom::array values;
  if (field.get(values)) {
    error = MarketDataError::Parse;
    return false;
  }
  std::size_t count = 0;
  for (auto item : values) {
    if (count >= out.size()) {
      error = MarketDataError::ArrayLength;
      return false;
    }
    std::uint64_t value = 0;
    std::int64_t signed_value = 0;
    if (!item.get(value)) {
      if (value > 0xffffffffULL) {
        error = MarketDataError::Parse;
        return false;
      }
    } else if (!item.get(signed_value) && signed_value >= 0) {
      value = static_cast<std::uint64_t>(signed_value);
    } else {
      error = MarketDataError::Parse;
      return false;
    }
    out[count++] = static_cast<std::uint32_t>(value);
  }
  if (expected != 0 && count != expected) {
    error = MarketDataError::ArrayLength;
    return false;
  }
  return true;
}

bool fill_u8_array(simdjson::dom::element field, std::span<std::uint8_t> out,
                   std::size_t expected, MarketDataError &error) {
  simdjson::dom::array values;
  if (field.get(values)) {
    error = MarketDataError::Parse;
    return false;
  }
  std::size_t count = 0;
  for (auto item : values) {
    if (count >= out.size()) {
      error = MarketDataError::ArrayLength;
      return false;
    }
    std::uint64_t value = 0;
    std::int64_t signed_value = 0;
    if (!item.get(value)) {
      if (value > 0xffU) {
        error = MarketDataError::Parse;
        return false;
      }
    } else if (!item.get(signed_value) && signed_value >= 0 &&
               signed_value <= 0xff) {
      value = static_cast<std::uint64_t>(signed_value);
    } else {
      error = MarketDataError::Parse;
      return false;
    }
    out[count++] = static_cast<std::uint8_t>(value);
  }
  if (expected != 0 && count != expected) {
    error = MarketDataError::ArrayLength;
    return false;
  }
  return true;
}

bool parse_nested_i64_array(simdjson::dom::element field,
                            std::span<std::array<std::int64_t, 8>> out,
                            std::size_t expected, MarketDataError &error) {
  simdjson::dom::array rows;
  if (field.get(rows)) {
    error = MarketDataError::Parse;
    return false;
  }
  std::size_t count = 0;
  for (auto row : rows) {
    if (count >= out.size()) {
      error = MarketDataError::ArrayLength;
      return false;
    }
    if (!fill_i64_array(row, out[count], 8, error)) {
      return false;
    }
    ++count;
  }
  if (expected != 0 && count != expected) {
    error = MarketDataError::ArrayLength;
    return false;
  }
  return true;
}

std::vector<std::string_view> split_lines(std::string_view body) {
  std::vector<std::string_view> lines;
  std::size_t start = 0;
  while (start < body.size()) {
    const auto end = body.find('\n', start);
    const auto line =
        body.substr(start, end == std::string_view::npos ? std::string_view::npos
                                                         : end - start);
    if (!line.empty() && line.back() == '\r') {
      lines.push_back(line.substr(0, line.size() - 1));
    } else if (!line.empty()) {
      lines.push_back(line);
    }
    if (end == std::string_view::npos) {
      break;
    }
    start = end + 1;
  }
  return lines;
}

bool parse_gated_side(simdjson::dom::object object, std::string_view prefix,
                      utils::md::wire::AggBboSide &side,
                      MarketDataError &error) {
  const std::string price = std::string(prefix) + "_price";
  const std::string quantity = std::string(prefix) + "_quantity";
  const std::string venue_quantity = std::string(prefix) + "_venue_quantity";
  const std::string exchange_ts = std::string(prefix) + "_exchange_ts_ns";
  const std::string venue_mask = std::string(prefix) + "_venue_mask";
  const std::string worst = std::string(prefix) + "_worst_ingress_age_us";
  const std::string best = std::string(prefix) + "_best_venue";
  const std::string timestamp = std::string(prefix) + "_timestamp_venue";
  const std::string contributors = std::string(prefix) + "_contributor_count";
  if (!get_i64(object, price, side.price) ||
      !get_i64(object, quantity, side.quantity) ||
      !get_u64(object, exchange_ts, side.exchange_ts_ns) ||
      !get_u32(object, venue_mask, side.venue_mask) ||
      !get_u32(object, worst, side.worst_ingress_age_us) ||
      !get_u8(object, best, side.best_venue) ||
      !get_u8(object, timestamp, side.timestamp_venue) ||
      !get_u8(object, contributors, side.contributor_count)) {
    error = MarketDataError::Parse;
    return false;
  }
  simdjson::dom::element venues;
  if (object[venue_quantity].get(venues)) {
    error = MarketDataError::Parse;
    return false;
  }
  return fill_i64_array(venues, side.venue_quantity, 8, error);
}

bool parse_raw_side(simdjson::dom::object object, std::string_view prefix,
                    utils::md::wire::AggBboRawSide &side,
                    MarketDataError &error) {
  const std::string price = std::string(prefix) + "_price";
  const std::string quantity = std::string(prefix) + "_quantity";
  const std::string venue_mask = std::string(prefix) + "_venue_mask";
  const std::string best = std::string(prefix) + "_best_venue";
  if (!get_i64(object, price, side.price) ||
      !get_i64(object, quantity, side.quantity) ||
      !get_u32(object, venue_mask, side.venue_mask) ||
      !get_u8(object, best, side.best_venue)) {
    error = MarketDataError::Parse;
    return false;
  }
  return true;
}

}  // namespace

std::string_view market_data_error_name(MarketDataError error) {
  switch (error) {
    case MarketDataError::Ok:
      return "ok";
    case MarketDataError::Timeout:
      return "timeout";
    case MarketDataError::Http:
      return "http";
    case MarketDataError::Auth:
      return "auth";
    case MarketDataError::Empty:
      return "empty";
    case MarketDataError::ArrayLength:
      return "array_length";
    case MarketDataError::Parse:
      return "parse";
  }
  return "unknown";
}

LiveSample live_sample_from_history_row(const HistoryBboRow &row) {
  LiveSample sample{};
  sample.wall_ns = row.sampled_at_ns;
  sample.raw_bps = signed_spread_bps(row.raw_bid_price, row.raw_ask_price);
  sample.gated_bps =
      signed_spread_bps(row.gated_bid_price, row.gated_ask_price);
  sample.raw_min = static_cast<double>(row.cross_window_raw_min);
  sample.raw_max = static_cast<double>(row.cross_window_raw_max);
  sample.gated_min = static_cast<double>(row.cross_window_gated_min);
  sample.gated_max = static_cast<double>(row.cross_window_gated_max);
  sample.window_samples = row.cross_window_samples;
  return sample;
}

MarketDataRepository::MarketDataRepository(ClickHouseReadConfig config,
                                           MarketDataQuery query)
    : config_(std::move(config)), query_(std::move(query)) {}

MarketDataQueryResult
MarketDataRepository::execute(std::string sql) const {
  last_sql_ = sql;
  ++query_count_;
  if (query_) {
    return query_(sql);
  }
  return http_query(sql);
}

MarketDataQueryResult
MarketDataRepository::http_query(std::string_view sql) const {
  MarketDataQueryResult result;
  if (config_.host.empty() || !identifier(config_.database) ||
      !identifier(config_.user) || !identifier(config_.catalog_table) ||
      !identifier(config_.bbo_table) || !identifier(config_.orderbook_table) ||
      config_.request_timeout_ms == 0) {
    result.error = MarketDataError::Http;
    result.message = "invalid ClickHouse reader configuration";
    return result;
  }
  const auto password = config_.password;
  std::string tls_error;
  auto tls = net::make_client_ssl_context(tls_error);
  if (!tls) {
    result.error = MarketDataError::Http;
    result.message = tls_error;
    return result;
  }
  std::string target = "/?database=" + url_encode(config_.database) +
                       "&user=" + url_encode(config_.user) +
                       "&password=" + url_encode(password) +
                       "&query=" + url_encode(sql);
  net::HttpClient client(std::move(tls), 16U << 10U, 32U << 20U,
                         std::max<std::size_t>(8U << 10U, target.size() + 256));
  const auto deadline =
      net::HttpClient::Clock::now() +
      std::chrono::milliseconds(config_.request_timeout_ms);
  if (!client.start_get(config_.host, config_.service, target, deadline)) {
    result.error = MarketDataError::Http;
    result.message = std::string(client.error_message());
    return result;
  }
  while (client.state() != net::HttpClientState::Complete) {
    if (client.state() == net::HttpClientState::TimedOut) {
      result.error = MarketDataError::Timeout;
      result.message = "ClickHouse query timed out";
      return result;
    }
    if (client.state() == net::HttpClientState::Failed) {
      result.error = MarketDataError::Http;
      result.message = std::string(client.error_message());
      return result;
    }
    pollfd descriptor{client.fd(), 0, 0};
    const auto wanted = client.wanted_events();
    if ((wanted & EPOLLIN) != 0) {
      descriptor.events |= POLLIN;
    }
    if ((wanted & EPOLLOUT) != 0) {
      descriptor.events |= POLLOUT;
    }
    const int ready = ::poll(&descriptor, 1, 50);
    std::uint32_t events{};
    if (ready > 0) {
      if ((descriptor.revents & POLLIN) != 0) {
        events |= EPOLLIN;
      }
      if ((descriptor.revents & POLLOUT) != 0) {
        events |= EPOLLOUT;
      }
      if ((descriptor.revents & (POLLERR | POLLHUP | POLLNVAL)) != 0) {
        events |= EPOLLERR | EPOLLHUP;
      }
    }
    (void)client.on_event(events);
    (void)client.check_timeout();
  }
  const auto status = client.response().status_code();
  if (client.response().status_class() != net::HttpStatusClass::Success) {
    result.error = classify_status(status);
    result.message = "ClickHouse HTTP status " + std::to_string(status);
    return result;
  }
  const auto body = client.response().body();
  result.body.assign(reinterpret_cast<const char *>(body.data()), body.size());
  return result;
}

MarketDataResult<std::vector<CatalogTopicRow>>
MarketDataRepository::query_catalog() const {
  MarketDataResult<std::vector<CatalogTopicRow>> result;
  const auto sql =
      "SELECT profile, symbol, kind, segment, active, schema_major, "
      "schema_minor, updated_at_ns FROM (SELECT profile, symbol, kind, "
      "segment, active, schema_major, schema_minor, updated_at_ns, "
      "service_instance FROM " +
      qualified(config_.database, config_.catalog_table) +
      " WHERE catalog_scope = " + sql_string(config_.catalog_scope) +
      " ORDER BY updated_at_ns DESC, service_instance DESC LIMIT 1 BY segment) "
      "WHERE active = 1 FORMAT JSONEachRow";
  const auto raw = execute(sql);
  if (raw.error != MarketDataError::Ok) {
    result.error = raw.error;
    result.message = raw.message;
    return result;
  }
  if (raw.body.empty()) {
    result.error = MarketDataError::Empty;
    result.message = "ClickHouse catalog is empty";
    return result;
  }
  simdjson::dom::parser parser;
  for (const auto line : split_lines(raw.body)) {
    simdjson::dom::element doc;
    if (parser.parse(line).get(doc)) {
      result.error = MarketDataError::Parse;
      result.message = "catalog JSON parse failed";
      return result;
    }
    simdjson::dom::object object;
    if (doc.get(object)) {
      result.error = MarketDataError::Parse;
      result.message = "catalog row is not an object";
      return result;
    }
    CatalogTopicRow row;
    std::uint64_t active = 1;
    if (!get_string(object, "profile", row.profile) ||
        !get_string(object, "symbol", row.symbol) ||
        !get_string(object, "kind", row.kind) ||
        !get_string(object, "segment", row.segment) ||
        !get_u16(object, "schema_major", row.schema_major) ||
        !get_u16(object, "schema_minor", row.schema_minor) ||
        !get_u64(object, "updated_at_ns", row.updated_at_ns)) {
      result.error = MarketDataError::Parse;
      result.message = "catalog row missing fields";
      return result;
    }
    bool active_flag = true;
    if (get_bool_u8(object, "active", active_flag)) {
      row.active = active_flag;
    } else if (get_u64(object, "active", active)) {
      row.active = active != 0;
    } else {
      result.error = MarketDataError::Parse;
      result.message = "catalog active field missing";
      return result;
    }
    result.value.push_back(std::move(row));
  }
  if (result.value.empty()) {
    result.error = MarketDataError::Empty;
    result.message = "ClickHouse catalog is empty";
  }
  return result;
}

MarketDataResult<std::vector<LatestBboRow>>
MarketDataRepository::query_latest_bbo(
    const std::vector<std::string> &segments) const {
  MarketDataResult<std::vector<LatestBboRow>> result;
  if (segments.empty()) {
    return result;
  }
  const auto sql =
      "SELECT toUnixTimestamp64Nano(sampled_at) AS sampled_at_ns, "
      "source_wall_ns, segment, ring_epoch, ring_sequence, generation, "
      "header_flags, header_exchange_ts_ns, base_asset, quote_asset, "
      "venue_slot_ids, price_scale, quantity_scale, member_count, member_mask, "
      "live_mask, gated_bid_price, gated_bid_quantity, "
      "gated_bid_venue_quantity, gated_bid_exchange_ts_ns, gated_bid_venue_mask, "
      "gated_bid_worst_ingress_age_us, gated_bid_best_venue, "
      "gated_bid_timestamp_venue, gated_bid_contributor_count, gated_ask_price, "
      "gated_ask_quantity, gated_ask_venue_quantity, gated_ask_exchange_ts_ns, "
      "gated_ask_venue_mask, gated_ask_worst_ingress_age_us, "
      "gated_ask_best_venue, gated_ask_timestamp_venue, "
      "gated_ask_contributor_count, raw_bid_price, raw_bid_quantity, "
      "raw_bid_venue_mask, raw_bid_best_venue, raw_ask_price, raw_ask_quantity, "
      "raw_ask_venue_mask, raw_ask_best_venue, raw_cross_bps, gated_cross_bps, "
      "skew_us, cross_skew_threshold_us, fx_age_us, cross_bid_venue, "
      "cross_ask_venue, fx_venue FROM " +
      qualified(config_.database, config_.bbo_table) + " WHERE segment IN " +
      sql_in_list(segments) +
      " ORDER BY sampled_at DESC LIMIT 1 BY segment FORMAT JSONEachRow";
  const auto raw = execute(sql);
  if (raw.error != MarketDataError::Ok) {
    result.error = raw.error;
    result.message = raw.message;
    return result;
  }
  if (raw.body.empty()) {
    return result;
  }
  simdjson::dom::parser parser;
  for (const auto line : split_lines(raw.body)) {
    simdjson::dom::element doc;
    if (parser.parse(line).get(doc)) {
      result.error = MarketDataError::Parse;
      result.message = "latest BBO JSON parse failed";
      return result;
    }
    simdjson::dom::object object;
    if (doc.get(object)) {
      result.error = MarketDataError::Parse;
      result.message = "latest BBO row is not an object";
      return result;
    }
    LatestBboRow row;
    std::string base;
    std::string quote;
    if (!get_u64(object, "sampled_at_ns", row.sampled_at_ns) ||
        !get_u64(object, "source_wall_ns", row.source_wall_ns) ||
        !get_string(object, "segment", row.segment) ||
        !get_u64(object, "ring_epoch", row.ring_epoch) ||
        !get_u64(object, "ring_sequence", row.ring_sequence) ||
        !get_u64(object, "generation", row.generation) ||
        !get_u16(object, "header_flags", row.record.header.flags) ||
        !get_u64(object, "header_exchange_ts_ns",
                 row.record.header.exchange_ts_ns) ||
        !get_string(object, "base_asset", base) ||
        !get_string(object, "quote_asset", quote) ||
        !get_u8(object, "price_scale", row.record.price_scale) ||
        !get_u8(object, "quantity_scale", row.record.quantity_scale) ||
        !get_u8(object, "member_count", row.record.member_count) ||
        !get_u32(object, "member_mask", row.record.member_mask) ||
        !get_u32(object, "live_mask", row.record.live_mask) ||
        !get_i32(object, "raw_cross_bps", row.record.raw_cross_bps) ||
        !get_i32(object, "gated_cross_bps", row.record.gated_cross_bps) ||
        !get_u32(object, "skew_us", row.record.skew_us) ||
        !get_u32(object, "cross_skew_threshold_us",
                 row.record.cross_skew_threshold_us) ||
        !get_u32(object, "fx_age_us", row.record.fx_age_us) ||
        !get_u8(object, "cross_bid_venue", row.record.cross_bid_venue) ||
        !get_u8(object, "cross_ask_venue", row.record.cross_ask_venue) ||
        !get_u8(object, "fx_venue", row.record.fx_venue)) {
      result.error = MarketDataError::Parse;
      result.message = "latest BBO row missing fields";
      return result;
    }
    copy_fixed(row.record.base_asset, base);
    copy_fixed(row.record.quote_asset, quote);
    simdjson::dom::element slots;
    if (object["venue_slot_ids"].get(slots) ||
        !fill_u8_array(slots, row.record.venue_slot_ids, 8, result.error)) {
      if (result.error == MarketDataError::Ok) {
        result.error = MarketDataError::Parse;
      }
      result.message = "latest BBO venue_slot_ids invalid";
      return result;
    }
    if (!parse_gated_side(object, "gated_bid", row.record.gated_bid,
                          result.error) ||
        !parse_gated_side(object, "gated_ask", row.record.gated_ask,
                          result.error) ||
        !parse_raw_side(object, "raw_bid", row.record.raw_bid, result.error) ||
        !parse_raw_side(object, "raw_ask", row.record.raw_ask, result.error)) {
      result.message = "latest BBO side fields invalid";
      return result;
    }
    result.value.push_back(std::move(row));
  }
  return result;
}

MarketDataResult<std::vector<LatestBookRow>>
MarketDataRepository::query_latest_book(
    const std::vector<std::string> &segments) const {
  MarketDataResult<std::vector<LatestBookRow>> result;
  if (segments.empty()) {
    return result;
  }
  const auto sql =
      "SELECT toUnixTimestamp64Nano(sampled_at) AS sampled_at_ns, "
      "source_wall_ns, segment, ring_epoch, ring_sequence, generation, "
      "header_flags, exchange_ts_ns, book_generation, base_asset, quote_asset, "
      "venue_slot_ids, price_scale, quantity_scale, member_count, member_mask, "
      "active_mask, bid_count, ask_count, bid_price, bid_quantity, "
      "bid_venue_mask, bid_contributor_count, bid_venue_quantity, ask_price, "
      "ask_quantity, ask_venue_mask, ask_contributor_count, ask_venue_quantity "
      "FROM " +
      qualified(config_.database, config_.orderbook_table) +
      " WHERE segment IN " + sql_in_list(segments) +
      " ORDER BY sampled_at DESC LIMIT 1 BY segment FORMAT JSONEachRow";
  const auto raw = execute(sql);
  if (raw.error != MarketDataError::Ok) {
    result.error = raw.error;
    result.message = raw.message;
    return result;
  }
  if (raw.body.empty()) {
    return result;
  }
  simdjson::dom::parser parser;
  for (const auto line : split_lines(raw.body)) {
    simdjson::dom::element doc;
    if (parser.parse(line).get(doc)) {
      result.error = MarketDataError::Parse;
      result.message = "latest OrderBook JSON parse failed";
      return result;
    }
    simdjson::dom::object object;
    if (doc.get(object)) {
      result.error = MarketDataError::Parse;
      result.message = "latest OrderBook row is not an object";
      return result;
    }
    LatestBookRow row;
    std::string base;
    std::string quote;
    std::uint32_t bid_count = 0;
    std::uint32_t ask_count = 0;
    std::uint32_t book_generation = 0;
    if (!get_u64(object, "sampled_at_ns", row.sampled_at_ns) ||
        !get_u64(object, "source_wall_ns", row.source_wall_ns) ||
        !get_string(object, "segment", row.segment) ||
        !get_u64(object, "ring_epoch", row.ring_epoch) ||
        !get_u64(object, "ring_sequence", row.ring_sequence) ||
        !get_u64(object, "generation", row.generation) ||
        !get_u16(object, "header_flags", row.record.header.flags) ||
        !get_u64(object, "exchange_ts_ns", row.record.header.exchange_ts_ns) ||
        !get_u32(object, "book_generation", book_generation) ||
        !get_string(object, "base_asset", base) ||
        !get_string(object, "quote_asset", quote) ||
        !get_u8(object, "price_scale", row.record.price_scale) ||
        !get_u8(object, "quantity_scale", row.record.quantity_scale) ||
        !get_u8(object, "member_count", row.record.member_count) ||
        !get_u32(object, "member_mask", row.record.member_mask) ||
        !get_u32(object, "active_mask", row.record.active_mask) ||
        !get_u32(object, "bid_count", bid_count) ||
        !get_u32(object, "ask_count", ask_count)) {
      result.error = MarketDataError::Parse;
      result.message = "latest OrderBook row missing fields";
      return result;
    }
    row.record.header.book_generation = book_generation;
    copy_fixed(row.record.base_asset, base);
    copy_fixed(row.record.quote_asset, quote);
    row.record.bid_count = static_cast<std::uint16_t>(
        std::min<std::uint32_t>(bid_count, utils::md::wire::kAggMaxLevelsPerSide));
    row.record.ask_count = static_cast<std::uint16_t>(
        std::min<std::uint32_t>(ask_count, utils::md::wire::kAggMaxLevelsPerSide));
    simdjson::dom::element slots;
    if (object["venue_slot_ids"].get(slots) ||
        !fill_u8_array(slots, row.record.venue_slot_ids, 8, result.error)) {
      if (result.error == MarketDataError::Ok) {
        result.error = MarketDataError::Parse;
      }
      result.message = "latest OrderBook venue_slot_ids invalid";
      return result;
    }
    std::array<std::int64_t, utils::md::wire::kAggMaxLevelsPerSide> bid_price{};
    std::array<std::int64_t, utils::md::wire::kAggMaxLevelsPerSide> bid_qty{};
    std::array<std::uint32_t, utils::md::wire::kAggMaxLevelsPerSide> bid_mask{};
    std::array<std::uint8_t, utils::md::wire::kAggMaxLevelsPerSide> bid_count_arr{};
    std::array<std::array<std::int64_t, 8>, utils::md::wire::kAggMaxLevelsPerSide>
        bid_venues{};
    std::array<std::int64_t, utils::md::wire::kAggMaxLevelsPerSide> ask_price{};
    std::array<std::int64_t, utils::md::wire::kAggMaxLevelsPerSide> ask_qty{};
    std::array<std::uint32_t, utils::md::wire::kAggMaxLevelsPerSide> ask_mask{};
    std::array<std::uint8_t, utils::md::wire::kAggMaxLevelsPerSide> ask_count_arr{};
    std::array<std::array<std::int64_t, 8>, utils::md::wire::kAggMaxLevelsPerSide>
        ask_venues{};
    simdjson::dom::element field;
    if (object["bid_price"].get(field) ||
        !fill_i64_array(field, bid_price, row.record.bid_count, result.error) ||
        object["bid_quantity"].get(field) ||
        !fill_i64_array(field, bid_qty, row.record.bid_count, result.error) ||
        object["bid_venue_mask"].get(field) ||
        !fill_u32_array(field, bid_mask, row.record.bid_count, result.error) ||
        object["bid_contributor_count"].get(field) ||
        !fill_u8_array(field, bid_count_arr, row.record.bid_count,
                       result.error) ||
        object["bid_venue_quantity"].get(field) ||
        !parse_nested_i64_array(field, bid_venues, row.record.bid_count,
                                result.error) ||
        object["ask_price"].get(field) ||
        !fill_i64_array(field, ask_price, row.record.ask_count, result.error) ||
        object["ask_quantity"].get(field) ||
        !fill_i64_array(field, ask_qty, row.record.ask_count, result.error) ||
        object["ask_venue_mask"].get(field) ||
        !fill_u32_array(field, ask_mask, row.record.ask_count, result.error) ||
        object["ask_contributor_count"].get(field) ||
        !fill_u8_array(field, ask_count_arr, row.record.ask_count,
                       result.error) ||
        object["ask_venue_quantity"].get(field) ||
        !parse_nested_i64_array(field, ask_venues, row.record.ask_count,
                                result.error)) {
      if (result.error == MarketDataError::Ok) {
        result.error = MarketDataError::Parse;
      }
      result.message = "latest OrderBook arrays invalid";
      return result;
    }
    for (std::uint16_t i = 0; i < row.record.bid_count; ++i) {
      auto &level = row.record.bids[i];
      level.price = bid_price[i];
      level.quantity = bid_qty[i];
      level.venue_mask = bid_mask[i];
      level.contributor_count = bid_count_arr[i];
      level.venue_quantity = bid_venues[i];
    }
    for (std::uint16_t i = 0; i < row.record.ask_count; ++i) {
      auto &level = row.record.asks[i];
      level.price = ask_price[i];
      level.quantity = ask_qty[i];
      level.venue_mask = ask_mask[i];
      level.contributor_count = ask_count_arr[i];
      level.venue_quantity = ask_venues[i];
    }
    result.value.push_back(std::move(row));
  }
  return result;
}

MarketDataResult<std::vector<HistoryBboRow>>
MarketDataRepository::query_bbo_history(std::string_view segment,
                                        std::uint64_t start_ns,
                                        std::uint64_t end_ns) const {
  MarketDataResult<std::vector<HistoryBboRow>> result;
  const auto sql =
      "SELECT toUnixTimestamp64Nano(sampled_at) AS sampled_at_ns, "
      "raw_bid_price, raw_ask_price, gated_bid_price, gated_ask_price, "
      "cross_window_raw_min, cross_window_raw_max, cross_window_gated_min, "
      "cross_window_gated_max, cross_window_samples FROM " +
      qualified(config_.database, config_.bbo_table) +
      " WHERE segment = " + sql_string(segment) +
      " AND sampled_at >= fromUnixTimestamp64Nano(" + std::to_string(start_ns) +
      ") AND sampled_at < fromUnixTimestamp64Nano(" + std::to_string(end_ns) +
      ") ORDER BY sampled_at ASC FORMAT JSONEachRow";
  const auto raw = execute(sql);
  if (raw.error != MarketDataError::Ok) {
    result.error = raw.error;
    result.message = raw.message;
    return result;
  }
  simdjson::dom::parser parser;
  for (const auto line : split_lines(raw.body)) {
    simdjson::dom::element doc;
    if (parser.parse(line).get(doc)) {
      result.error = MarketDataError::Parse;
      result.message = "history JSON parse failed";
      return result;
    }
    simdjson::dom::object object;
    if (doc.get(object)) {
      result.error = MarketDataError::Parse;
      result.message = "history row is not an object";
      return result;
    }
    HistoryBboRow row;
    if (!get_u64(object, "sampled_at_ns", row.sampled_at_ns) ||
        !get_i64(object, "raw_bid_price", row.raw_bid_price) ||
        !get_i64(object, "raw_ask_price", row.raw_ask_price) ||
        !get_i64(object, "gated_bid_price", row.gated_bid_price) ||
        !get_i64(object, "gated_ask_price", row.gated_ask_price) ||
        !get_i32(object, "cross_window_raw_min", row.cross_window_raw_min) ||
        !get_i32(object, "cross_window_raw_max", row.cross_window_raw_max) ||
        !get_i32(object, "cross_window_gated_min",
                 row.cross_window_gated_min) ||
        !get_i32(object, "cross_window_gated_max",
                 row.cross_window_gated_max) ||
        !get_u64(object, "cross_window_samples", row.cross_window_samples)) {
      result.error = MarketDataError::Parse;
      result.message = "history row missing fields";
      return result;
    }
    if (!result.value.empty() &&
        result.value.back().sampled_at_ns == row.sampled_at_ns) {
      result.value.back() = row;
      continue;
    }
    result.value.push_back(row);
  }
  return result;
}

MarketDataResult<std::vector<HistoryOhlcRow>>
MarketDataRepository::query_bbo_history_ohlc(std::string_view segment,
                                             std::uint64_t start_ns,
                                             std::uint64_t end_ns,
                                             std::uint64_t resolution_ns,
                                             std::string_view bps_type) const {
  MarketDataResult<std::vector<HistoryOhlcRow>> result;
  if (resolution_ns == 0 || end_ns <= start_ns) {
    return result;
  }
  const auto spread = signed_spread_sql(bps_type);
  const auto sql =
      "SELECT " + std::to_string(start_ns) +
      " + intDiv(toUnixTimestamp64Nano(sampled_at) - " +
      std::to_string(start_ns) + ", " + std::to_string(resolution_ns) +
      ") * " + std::to_string(resolution_ns) +
      " AS bucket_start_ns, argMin(bps, sampled_at) AS open, max(bps) AS high, "
      "min(bps) AS low, argMax(bps, sampled_at) AS close, " +
      window_min_sql(bps_type) + " AS window_min, " + window_max_sql(bps_type) +
      " AS window_max, count() AS samples FROM (SELECT sampled_at, " + spread +
      " AS bps, cross_window_raw_min, cross_window_raw_max, "
      "cross_window_gated_min, cross_window_gated_max FROM " +
      qualified(config_.database, config_.bbo_table) +
      " WHERE segment = " + sql_string(segment) +
      " AND sampled_at >= fromUnixTimestamp64Nano(" + std::to_string(start_ns) +
      ") AND sampled_at < fromUnixTimestamp64Nano(" + std::to_string(end_ns) +
      ")) GROUP BY bucket_start_ns ORDER BY bucket_start_ns ASC FORMAT JSONEachRow";
  const auto raw = execute(sql);
  if (raw.error != MarketDataError::Ok) {
    result.error = raw.error;
    result.message = raw.message;
    return result;
  }
  simdjson::dom::parser parser;
  for (const auto line : split_lines(raw.body)) {
    simdjson::dom::element doc;
    if (parser.parse(line).get(doc)) {
      result.error = MarketDataError::Parse;
      result.message = "ohlc JSON parse failed";
      return result;
    }
    simdjson::dom::object object;
    if (doc.get(object)) {
      result.error = MarketDataError::Parse;
      result.message = "ohlc row is not an object";
      return result;
    }
    HistoryOhlcRow row;
    if (!get_u64(object, "bucket_start_ns", row.bucket_start_ns) ||
        !get_f64(object, "open", row.open) ||
        !get_f64(object, "high", row.high) || !get_f64(object, "low", row.low) ||
        !get_f64(object, "close", row.close) ||
        !get_f64(object, "window_min", row.window_min) ||
        !get_f64(object, "window_max", row.window_max) ||
        !get_u64(object, "samples", row.samples)) {
      result.error = MarketDataError::Parse;
      result.message = "ohlc row missing fields";
      return result;
    }
    result.value.push_back(row);
  }
  return result;
}

MarketDataResult<std::vector<double>>
MarketDataRepository::query_bbo_history_distribution(
    std::string_view segment, std::uint64_t start_ns, std::uint64_t end_ns,
    std::string_view bps_type) const {
  MarketDataResult<std::vector<double>> result;
  const auto spread = signed_spread_sql(bps_type);
  const auto sql =
      "WITH src AS (SELECT " + spread + " AS bps FROM " +
      qualified(config_.database, config_.bbo_table) +
      " WHERE segment = " + sql_string(segment) +
      " AND sampled_at >= fromUnixTimestamp64Nano(" + std::to_string(start_ns) +
      ") AND sampled_at < fromUnixTimestamp64Nano(" + std::to_string(end_ns) +
      ")), stats AS (SELECT count() AS n FROM src), "
      "sorted AS (SELECT bps, row_number() OVER (ORDER BY bps) AS rn FROM src) "
      "SELECT bps FROM ("
      "SELECT 0 AS part, rn AS ord, bps FROM sorted "
      "WHERE (SELECT n FROM stats) <= 10000 UNION ALL "
      "SELECT 1 AS part, toUInt64(idx.i) AS ord, s.bps "
      "FROM (SELECT arrayJoin(range(10000)) AS i) AS idx CROSS JOIN stats "
      "INNER JOIN sorted AS s ON s.rn = toUInt64(toUInt64(1) + intDiv("
      "toUInt64(idx.i) * (stats.n - toUInt64(1)), toUInt64(9999))) "
      "WHERE stats.n > 10000) ORDER BY part, ord FORMAT JSONEachRow";
  const auto raw = execute(sql);
  if (raw.error != MarketDataError::Ok) {
    result.error = raw.error;
    result.message = raw.message;
    return result;
  }
  simdjson::dom::parser parser;
  for (const auto line : split_lines(raw.body)) {
    simdjson::dom::element doc;
    if (parser.parse(line).get(doc)) {
      result.error = MarketDataError::Parse;
      result.message = "distribution JSON parse failed";
      return result;
    }
    simdjson::dom::object object;
    if (doc.get(object)) {
      result.error = MarketDataError::Parse;
      result.message = "distribution row is not an object";
      return result;
    }
    double bps = 0;
    if (!get_f64(object, "bps", bps)) {
      result.error = MarketDataError::Parse;
      result.message = "distribution row missing bps";
      return result;
    }
    result.value.push_back(bps);
  }
  return result;
}

}  // namespace fairprice

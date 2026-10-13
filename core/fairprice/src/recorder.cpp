#include "fairprice/recorder.h"

#include "fairprice/engine.h"
#include "fairprice/json.h"

#include <algorithm>
#include <cctype>
#include <chrono>
#include <cstdio>
#include <ctime>
#include <cstring>
#include <libpq-fe.h>
#include <limits>
#include <sstream>
#include <vector>

namespace fairprice {
namespace {

void sleep_ms(std::uint64_t ms, std::atomic<bool> &stop) {
  const auto end =
      std::chrono::steady_clock::now() + std::chrono::milliseconds(ms);
  while (!stop.load() && std::chrono::steady_clock::now() < end) {
    std::this_thread::sleep_for(std::chrono::milliseconds(10));
  }
}

PGconn *connect(const std::string &url, std::uint64_t timeout_ms) {
  auto *conn = PQconnectdb(url.c_str());
  if (!conn || PQstatus(conn) != CONNECTION_OK) {
    const char *message = conn ? PQerrorMessage(conn) : "PQconnectdb returned null";
    std::fprintf(stderr, "fairprice: postgres connect failed error=%s", message);
    if (conn) {
      PQfinish(conn);
    }
    return nullptr;
  }
  if (timeout_ms > 0) {
    const auto seconds = std::max<std::uint64_t>(1, timeout_ms / 1000);
    const auto command = "SET statement_timeout TO " + std::to_string(seconds * 1000);
    auto *result = PQexec(conn, command.c_str());
    if (result) {
      PQclear(result);
    }
  }
  return conn;
}

bool parse_rfc3339(std::string_view text, std::chrono::system_clock::time_point &out) {
  if (text.size() < 20) {
    return false;
  }
  std::tm utc{};
  int year = 0;
  int month = 0;
  int day = 0;
  int hour = 0;
  int minute = 0;
  int second = 0;
  if (std::sscanf(text.data(), "%d-%d-%dT%d:%d:%d", &year, &month, &day, &hour,
                  &minute, &second) != 6) {
    return false;
  }
  utc.tm_year = year - 1900;
  utc.tm_mon = month - 1;
  utc.tm_mday = day;
  utc.tm_hour = hour;
  utc.tm_min = minute;
  utc.tm_sec = second;
  const auto seconds = timegm(&utc);
  if (seconds < 0) {
    return false;
  }
  auto tp = std::chrono::system_clock::from_time_t(seconds);
  const auto dot = text.find('.');
  if (dot != std::string_view::npos) {
    std::uint64_t frac = 0;
    std::size_t digits = 0;
    for (std::size_t i = dot + 1; i < text.size() && std::isdigit(static_cast<unsigned char>(text[i]));
         ++i) {
      frac = frac * 10 + static_cast<std::uint64_t>(text[i] - '0');
      ++digits;
    }
    while (digits < 9) {
      frac *= 10;
      ++digits;
    }
    while (digits > 9) {
      frac /= 10;
      --digits;
    }
    tp += std::chrono::nanoseconds(frac);
  }
  out = tp;
  return true;
}

std::int64_t resolution_ms(std::string_view value, std::chrono::nanoseconds span,
                           std::string &error) {
  std::string trimmed(value);
  while (!trimmed.empty() && (trimmed.front() == ' ' || trimmed.back() == ' ')) {
    if (trimmed.front() == ' ') {
      trimmed.erase(trimmed.begin());
    } else {
      trimmed.pop_back();
    }
  }
  if (trimmed.empty() || trimmed == "auto") {
    if (span <= std::chrono::minutes(10)) {
      return 1000;
    }
    if (span <= std::chrono::minutes(30)) {
      return 5000;
    }
    if (span <= std::chrono::hours(2)) {
      return 15000;
    }
    return 60000;
  }
  if (trimmed == "1s") {
    return 1000;
  }
  if (trimmed == "5s") {
    return 5000;
  }
  if (trimmed == "15s") {
    return 15000;
  }
  if (trimmed == "1m") {
    return 60000;
  }
  error = "resolution must be auto, 1s, 5s, 15s, or 1m";
  return 0;
}

std::string format_rfc3339(std::chrono::system_clock::time_point tp) {
  const auto seconds = std::chrono::time_point_cast<std::chrono::seconds>(tp);
  const auto ns = std::chrono::duration_cast<std::chrono::nanoseconds>(tp - seconds);
  const auto time = std::chrono::system_clock::to_time_t(seconds);
  std::tm utc{};
  gmtime_r(&time, &utc);
  char buffer[64]{};
  std::snprintf(buffer, sizeof(buffer), "%04d-%02d-%02dT%02d:%02d:%02d.%09lldZ",
                utc.tm_year + 1900, utc.tm_mon + 1, utc.tm_mday, utc.tm_hour,
                utc.tm_min, utc.tm_sec, static_cast<long long>(ns.count()));
  return buffer;
}

}  // namespace

std::string fixed_decimal_string(std::int64_t mantissa, std::uint8_t scale) {
  const bool negative = mantissa < 0;
  std::uint64_t abs_value = 0;
  if (mantissa == std::numeric_limits<std::int64_t>::min()) {
    abs_value = static_cast<std::uint64_t>(std::numeric_limits<std::int64_t>::max()) + 1U;
  } else {
    abs_value = static_cast<std::uint64_t>(negative ? -mantissa : mantissa);
  }
  auto digits = std::to_string(abs_value);
  if (scale > 0) {
    if (digits.size() <= scale) {
      digits = std::string(scale - digits.size() + 1, '0') + digits;
    }
    const auto index = digits.size() - scale;
    digits.insert(index, ".");
  }
  return negative ? "-" + digits : digits;
}

std::string postgres_text_array_from_mask(std::uint32_t mask) {
  std::string out = "{";
  append_degraded_reasons(mask, out);
  out += '}';
  return out;
}

std::string history_degraded_reasons_json_field(
    std::string_view array_to_json_text) {
  if (array_to_json_text.size() >= 2 && array_to_json_text.front() == '[' &&
      array_to_json_text.back() == ']' && array_to_json_text != "[]") {
    return std::string(array_to_json_text);
  }
  return "null";
}

SampleTickPlan next_sample_tick(std::uint64_t now_mono_ns,
                                std::uint64_t interval_ns,
                                std::uint64_t next_tick_ns) {
  SampleTickPlan plan;
  plan.sleep_until_mono_ns = next_tick_ns;
  if (interval_ns == 0 || now_mono_ns <= next_tick_ns) {
    return plan;
  }
  while (plan.sleep_until_mono_ns <= now_mono_ns) {
    plan.sleep_until_mono_ns += interval_ns;
    ++plan.missed;
  }
  return plan;
}

int validate_fair_price_history(std::string_view start_rfc3339,
                                std::string_view end_rfc3339,
                                std::string_view resolution, std::string &error) {
  std::chrono::system_clock::time_point start;
  std::chrono::system_clock::time_point end;
  if (!parse_rfc3339(start_rfc3339, start) || !parse_rfc3339(end_rfc3339, end) ||
      !(start < end)) {
    error =
        "start and end must be valid RFC3339 timestamps with start before end";
    return 400;
  }
  if (end - start > std::chrono::hours(24)) {
    error = "fair price history range must not exceed 24h";
    return 400;
  }
  if (resolution_ms(resolution, end - start, error) <= 0) {
    return 400;
  }
  return 0;
}

bool should_retry_history(int status) noexcept { return status == 503; }

Recorder::Recorder(Store *store, PersistenceConfig config,
                   std::string database_url)
    : store_(store), config_(std::move(config)),
      database_url_(std::move(database_url)) {}

Recorder::~Recorder() { stop(); }

void Recorder::start() {
  if (!config_.enabled || database_url_.empty()) {
    return;
  }
  stop_ = false;
  sample_thread_ = std::thread([this] { sample_loop(); });
  write_thread_ = std::thread([this] { write_loop(); });
}

void Recorder::stop() {
  stop_ = true;
  if (sample_thread_.joinable()) {
    sample_thread_.join();
  }
  if (write_thread_.joinable()) {
    write_thread_.join();
  }
}

void Recorder::sample_loop() {
  const auto interval_ns = config_.sample_interval_ms * 1'000'000ULL;
  auto now_mono = [] {
    return static_cast<std::uint64_t>(
        std::chrono::duration_cast<std::chrono::nanoseconds>(
            std::chrono::steady_clock::now().time_since_epoch())
            .count());
  };
  auto next_tick = now_mono() + interval_ns;
  PGconn *conn = nullptr;
  std::uint64_t insert_failures = 0;
  std::uint64_t last_insert_log_ns = 0;
  std::uint64_t connect_failures = 0;
  std::uint64_t last_connect_log_ns = 0;
  while (!stop_.load()) {
    const auto remain_ns =
        next_tick > now_mono() ? next_tick - now_mono() : 0;
    if (remain_ns > 0) {
      sleep_ms((remain_ns + 999'999ULL) / 1'000'000ULL, stop_);
    }
    if (stop_.load()) {
      break;
    }
    if (!store_) {
      continue;
    }
    const auto markets = store_->copy_all();
    const auto now = static_cast<std::uint64_t>(
        std::chrono::duration_cast<std::chrono::nanoseconds>(
            std::chrono::system_clock::now().time_since_epoch())
            .count());
    if (!conn) {
      conn = connect(database_url_, config_.operation_timeout_ms);
      if (!conn) {
        ++connect_failures;
        const auto stamp = now_mono();
        if (connect_failures == 1 ||
            stamp - last_connect_log_ns >= 10'000'000'000ULL) {
          last_connect_log_ns = stamp;
        }
      }
    }
    if (conn) {
    const auto observed_sec =
        static_cast<std::time_t>(now / 1'000'000'000ULL);
    std::tm utc{};
    gmtime_r(&observed_sec, &utc);
    char observed[32]{};
    std::strftime(observed, sizeof(observed), "%Y-%m-%d %H:%M:%S", &utc);
    for (const auto &market : markets) {
      if (!market.fair.ready ||
          !store_->fresh(market.fair.wall_ns, now)) {
        continue;
      }
      const auto &pod = market.fair;
      const auto price = fixed_decimal_string(pod.price, pod.output_scale);
      const auto price_raw = fixed_decimal_string(pod.price_raw, pod.output_scale);
      const auto mid = fixed_decimal_string(pod.mid, pod.output_scale);
      const auto micro = fixed_decimal_string(pod.microprice, pod.output_scale);
      const auto best_bid = fixed_decimal_string(pod.best_bid, pod.output_scale);
      const auto best_ask = fixed_decimal_string(pod.best_ask, pod.output_scale);
      const auto eff_bid =
          fixed_decimal_string(pod.effective_bid, pod.output_scale);
      const auto eff_ask =
          fixed_decimal_string(pod.effective_ask, pod.output_scale);
      const auto model = std::string(cstr_view(pod.model_id));
      const auto epoch = std::to_string(pod.ring_epoch);
      const auto sequence = std::to_string(pod.ring_sequence);
      const auto generation = std::to_string(pod.generation);
      const auto wall = std::to_string(pod.wall_ns);
      const auto exchange = std::to_string(pod.exchange_ts_ns);
      const auto price_scale = std::to_string(pod.price_scale);
      const auto quantity_scale = std::to_string(pod.quantity_scale);
      const auto imbalance = std::to_string(pod.imbalance);
      const auto spread = std::to_string(pod.spread_bps);
      const auto weighted_bid = std::to_string(pod.weighted_bid_depth);
      const auto weighted_ask = std::to_string(pod.weighted_ask_depth);
      const auto depth_bid = std::to_string(pod.depth_bid_notional);
      const auto depth_ask = std::to_string(pod.depth_ask_notional);
      const auto mask = std::to_string(pod.active_venue_mask);
      const auto degraded_reasons =
          postgres_text_array_from_mask(pod.degraded_mask);
      const char *values[] = {
          market.catalog.profile.c_str(),
          market.catalog.symbol.c_str(),
          model.c_str(),
          observed,
          epoch.c_str(),
          sequence.c_str(),
          generation.c_str(),
          wall.c_str(),
          exchange.c_str(),
          price_scale.c_str(),
          quantity_scale.c_str(),
          price.c_str(),
          price_raw.c_str(),
          mid.c_str(),
          micro.c_str(),
          best_bid.c_str(),
          best_ask.c_str(),
          eff_bid.c_str(),
          eff_ask.c_str(),
          imbalance.c_str(),
          spread.c_str(),
          weighted_bid.c_str(),
          weighted_ask.c_str(),
          depth_bid.c_str(),
          depth_ask.c_str(),
          mask.c_str(),
          pod.crossed ? "true" : "false",
          pod.degraded ? "true" : "false",
          degraded_reasons.c_str(),
      };
      auto *result = PQexecParams(
          conn,
          "INSERT INTO fair_price_snapshots ("
          "profile,symbol,model_id,observed_at,ring_epoch,ring_sequence,"
          "generation,source_wall_ns,exchange_ts_ns,price_scale,quantity_scale,"
          "price,price_raw,mid,microprice,best_bid,best_ask,effective_bid,"
          "effective_ask,imbalance,spread_bps,weighted_bid_depth,"
          "weighted_ask_depth,depth_bid_notional,depth_ask_notional,"
          "active_venue_mask,crossed,degraded,degraded_reasons) VALUES ("
          "$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,"
          "$20,$21,$22,$23,$24,$25,$26,$27,$28,$29::text[]) "
          "ON CONFLICT (profile,symbol,model_id,observed_at) DO UPDATE SET "
          "ring_epoch=EXCLUDED.ring_epoch,"
          "ring_sequence=EXCLUDED.ring_sequence,"
          "generation=EXCLUDED.generation,"
          "source_wall_ns=EXCLUDED.source_wall_ns,"
          "crossed=EXCLUDED.crossed,"
          "degraded=EXCLUDED.degraded,"
          "degraded_reasons=EXCLUDED.degraded_reasons "
          "WHERE EXCLUDED.source_wall_ns > fair_price_snapshots.source_wall_ns",
          29, nullptr, values, nullptr, nullptr, 0);
      const bool ok =
          result && PQresultStatus(result) == PGRES_COMMAND_OK;
      if (!ok) {
        ++insert_failures;
        const auto stamp = now_mono();
        if (insert_failures == 1 ||
            stamp - last_insert_log_ns >= 10'000'000'000ULL) {
          last_insert_log_ns = stamp;
          const char *sqlstate =
              result ? PQresultErrorField(result, PG_DIAG_SQLSTATE) : nullptr;
          const char *message =
              result ? PQresultErrorMessage(result) : PQerrorMessage(conn);
          std::fprintf(stderr,
                       "fairprice: postgres insert failed profile=%s symbol=%s "
                       "sqlstate=%s error=%s\n",
                       market.catalog.profile.c_str(),
                       market.catalog.symbol.c_str(),
                       sqlstate ? sqlstate : "", message ? message : "");
        }
        if (PQstatus(conn) != CONNECTION_OK) {
          PQfinish(conn);
          conn = nullptr;
        }
      } else if (insert_failures > 0) {
        std::fprintf(stderr,
                     "fairprice: postgres insert recovered failures=%llu "
                     "profile=%s symbol=%s\n",
                     static_cast<unsigned long long>(insert_failures),
                     market.catalog.profile.c_str(),
                     market.catalog.symbol.c_str());
        insert_failures = 0;
      }
      if (result) {
        PQclear(result);
      }
      if (!conn) {
        break;
      }
    }
    }
    auto plan = next_sample_tick(now_mono(), interval_ns, next_tick + interval_ns);
    if (plan.missed > 0) {
      std::fprintf(stderr,
                   "fairprice: sample_tick_missed count=%llu interval_ms=%llu\n",
                   static_cast<unsigned long long>(plan.missed),
                   static_cast<unsigned long long>(config_.sample_interval_ms));
    }
    next_tick = plan.sleep_until_mono_ns;
  }
  if (conn) {
    PQfinish(conn);
  }
}

void Recorder::write_loop() {
  while (!stop_.load()) {
    sleep_ms(config_.cleanup_interval_ms, stop_);
    if (stop_.load() || database_url_.empty()) {
      continue;
    }
    auto *conn = connect(database_url_, config_.operation_timeout_ms);
    if (!conn) {
      continue;
    }
    const auto cutoff = std::chrono::system_clock::now() -
                        std::chrono::hours(config_.retention_hours);
    const auto cutoff_time = std::chrono::system_clock::to_time_t(cutoff);
    std::tm utc{};
    gmtime_r(&cutoff_time, &utc);
    char cutoff_text[32]{};
    std::strftime(cutoff_text, sizeof(cutoff_text), "%Y-%m-%d %H:%M:%S", &utc);
    const auto limit = std::to_string(config_.delete_batch);
    for (int batch = 0; batch < config_.delete_max_batches && !stop_.load();
         ++batch) {
      const char *values[] = {cutoff_text, limit.c_str()};
      auto *result = PQexecParams(
          conn,
          "DELETE FROM fair_price_snapshots WHERE ctid IN ("
          "SELECT ctid FROM fair_price_snapshots WHERE observed_at < $1 "
          "ORDER BY observed_at LIMIT $2)",
          2, nullptr, values, nullptr, nullptr, 0);
      const bool ok =
          result && PQresultStatus(result) == PGRES_COMMAND_OK;
      const auto affected =
          ok ? std::strtoll(PQcmdTuples(result), nullptr, 10) : 0;
      if (!ok) {
        const char *sqlstate =
            result ? PQresultErrorField(result, PG_DIAG_SQLSTATE) : nullptr;
        const char *message =
            result ? PQresultErrorMessage(result) : PQerrorMessage(conn);
        std::fprintf(stderr,
                     "fairprice: postgres cleanup failed sqlstate=%s error=%s\n",
                     sqlstate ? sqlstate : "", message ? message : "");
      }
      if (result) {
        PQclear(result);
      }
      if (!ok || affected <= 0) {
        break;
      }
    }
    PQfinish(conn);
  }
}

bool Recorder::upsert(const std::vector<Market *> &, std::uint64_t) {
  return true;
}

std::string Recorder::query_history_json(std::string_view profile,
                                         std::string_view symbol,
                                         std::string_view model_id,
                                         std::string_view start_rfc3339,
                                         std::string_view end_rfc3339,
                                         std::string_view resolution,
                                         int &status) {
  if (!config_.enabled || database_url_.empty()) {
    status = 503;
    return encode_error_json("fair price history is unavailable");
  }
  std::chrono::system_clock::time_point start;
  std::chrono::system_clock::time_point end;
  if (!parse_rfc3339(start_rfc3339, start) || !parse_rfc3339(end_rfc3339, end) ||
      !(start < end)) {
    status = 400;
    return encode_error_json(
        "start and end must be valid RFC3339 timestamps with start before end");
  }
  if (end - start > std::chrono::hours(24)) {
    status = 400;
    return encode_error_json("fair price history range must not exceed 24h");
  }
  std::string error;
  const auto millis = resolution_ms(resolution, end - start, error);
  if (millis <= 0) {
    status = 400;
    return encode_error_json(error);
  }
  const auto start_text = format_rfc3339(start);
  const auto end_text = format_rfc3339(end);
  const auto interval = std::to_string(millis) + " milliseconds";
  const auto profile_text = std::string(profile);
  const auto symbol_text = std::string(symbol);
  const auto model_text = std::string(model_id);
  const char *values[] = {profile_text.c_str(), symbol_text.c_str(),
                          model_text.c_str(), start_text.c_str(),
                          end_text.c_str(), interval.c_str()};
  constexpr const char *kSql =
      "WITH sampled AS ("
      "SELECT observed_at, ring_epoch::text AS ring_epoch, "
      "ring_sequence::text AS ring_sequence, source_wall_ns, "
      "ROUND(price * POWER(10::numeric, LEAST(price_scale + 3, 18)), 0)"
      "::numeric(38, 0)::text AS price_mantissa, "
      "LEAST(price_scale + 3, 18) AS fair_price_scale, "
      "degraded, degraded_reasons, "
      "ROW_NUMBER() OVER ("
      "PARTITION BY date_bin($6::interval, observed_at, "
      "TIMESTAMPTZ '1970-01-01 00:00:00+00') "
      "ORDER BY observed_at DESC) AS bucket_rank "
      "FROM fair_price_snapshots "
      "WHERE profile=$1 AND symbol=$2 AND model_id=$3 "
      "AND observed_at >= $4 AND observed_at <= $5) "
      "SELECT observed_at, ring_epoch, ring_sequence, source_wall_ns, "
      "price_mantissa, fair_price_scale, degraded, "
      "array_to_json(degraded_reasons)::text AS degraded_reasons_json "
      "FROM sampled WHERE bucket_rank=1 ORDER BY observed_at ASC LIMIT 2000";
  PGconn *conn = nullptr;
  PGresult *result = nullptr;
  bool retried = false;
  for (int attempt = 0; attempt < 2; ++attempt) {
    if (conn) {
      PQfinish(conn);
      conn = nullptr;
    }
    conn = connect(database_url_, config_.operation_timeout_ms);
    const char *failure = nullptr;
    if (!conn) {
      failure = "connect failed";
    } else {
      result = PQexecParams(conn, kSql, 6, nullptr, values, nullptr, nullptr, 0);
      if (!result || PQresultStatus(result) != PGRES_TUPLES_OK) {
        failure = result ? PQresultErrorMessage(result) : PQerrorMessage(conn);
        if (result) {
          PQclear(result);
          result = nullptr;
        }
      }
    }
    if (!failure) {
      if (retried) {
        std::fprintf(stderr,
                     "fairprice: fair-price-history query recovered after retry\n");
      }
      break;
    }
    const bool retry = attempt == 0 && should_retry_history(503);
    std::fprintf(stderr,
                 "fairprice: fair-price-history query failed attempt=%d "
                 "retry=%s error=%s\n",
                 attempt + 1, retry ? "yes" : "no", failure);
    if (!retry) {
      if (conn) {
        PQfinish(conn);
      }
      status = 503;
      return encode_error_json("fair price history is unavailable");
    }
    retried = true;
  }
  std::string out = "{\"profile\":\"";
  out += json_escape(profile);
  out += "\",\"symbol\":\"";
  out += json_escape(symbol);
  out += "\",\"model_id\":\"";
  out += json_escape(model_id);
  out += "\",\"start\":\"";
  out += json_escape(start_text);
  out += "\",\"end\":\"";
  out += json_escape(end_text);
  out += "\",\"resolution_ms\":";
  append_i64(out, millis);
  out += ",\"points\":[";
  const int rows = PQntuples(result);
  for (int i = 0; i < rows; ++i) {
    if (i) {
      out += ',';
    }
    out += "{\"observed_at\":\"";
    out += json_escape(PQgetvalue(result, i, 0));
    out += "\",\"source_wall_ns\":\"";
    out += json_escape(PQgetvalue(result, i, 3));
    out += "\",\"ring_epoch\":\"";
    out += json_escape(PQgetvalue(result, i, 1));
    out += "\",\"seq\":\"";
    out += json_escape(PQgetvalue(result, i, 2));
    out += "\",\"price\":{\"mantissa\":\"";
    out += json_escape(PQgetvalue(result, i, 4));
    out += "\",\"scale\":";
    out += PQgetvalue(result, i, 5);
    out += "},\"degraded\":";
    out += (std::string_view(PQgetvalue(result, i, 6)) == "t") ? "true" : "false";
    out += ",\"degraded_reasons\":";
    out += history_degraded_reasons_json_field(
        std::string_view(PQgetvalue(result, i, 7)));
    out += '}';
  }
  out += "]}\n";
  PQclear(result);
  PQfinish(conn);
  status = 200;
  return out;
}

}  // namespace fairprice

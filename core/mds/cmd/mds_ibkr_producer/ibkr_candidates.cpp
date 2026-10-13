#include "ibkr_candidates.h"

#include <libpq-fe.h>

#include <algorithm>
#include <cctype>
#include <cerrno>
#include <cmath>
#include <cstdlib>
#include <iomanip>
#include <memory>
#include <set>
#include <sstream>
#include <string>

namespace mds::ibkr {
namespace {

constexpr std::string_view kCandidateQuery =
    "SELECT COALESCE(NULLIF(metadata->>'secType', ''), 'STK'), "
    "base_asset, quote_asset, ib_con_id::text, "
    "COALESCE(primary_exchange, '') "
    "FROM instruments "
    "WHERE exchange = 'ibkr' AND contract_type = 'spot' AND active "
    "AND status = 'active' AND ib_con_id > 0 AND base_asset <> '' "
    "ORDER BY ib_con_id";

constexpr std::string_view kTurnoverUpdateQuery =
    "UPDATE instruments "
    "SET turnover_usd_90d = $2::double precision, "
    "ib_metrics_observed_at = now(), updated_at = now() "
    "WHERE exchange = 'ibkr' AND ib_con_id = $1::bigint";

struct ConnectionDeleter {
  void operator()(PGconn *connection) const noexcept {
    if (connection != nullptr) {
      PQfinish(connection);
    }
  }
};

struct ResultDeleter {
  void operator()(PGresult *result) const noexcept {
    if (result != nullptr) {
      PQclear(result);
    }
  }
};

using Connection = std::unique_ptr<PGconn, ConnectionDeleter>;
using Result = std::unique_ptr<PGresult, ResultDeleter>;

std::string upper(std::string value) {
  for (char &character : value) {
    character =
        static_cast<char>(std::toupper(static_cast<unsigned char>(character)));
  }
  return value;
}

bool valid_symbol(std::string_view symbol) {
  if (symbol.empty() || symbol.size() >= 32 ||
      symbol.find('|') != std::string_view::npos) {
    return false;
  }
  return std::all_of(symbol.begin(), symbol.end(), [](unsigned char value) {
    return std::isalnum(value) != 0 || value == '.' || value == '-';
  });
}

bool parse_con_id(const char *text, std::int64_t &value) {
  if (text == nullptr || text[0] == '\0') {
    return false;
  }
  errno = 0;
  char *end = nullptr;
  const auto parsed = std::strtoll(text, &end, 10);
  if (errno != 0 || end == text || *end != '\0' || parsed <= 0) {
    return false;
  }
  value = parsed;
  return true;
}

bool candidate_from_row(PGresult *result, int row, InstrumentSpec &spec) {
  auto sec_type = upper(PQgetvalue(result, row, 0));
  auto base = upper(PQgetvalue(result, row, 1));
  auto quote = upper(PQgetvalue(result, row, 2));
  auto primary = upper(PQgetvalue(result, row, 4));
  std::int64_t con_id = 0;
  if (!parse_con_id(PQgetvalue(result, row, 3), con_id)) {
    return false;
  }
  if (sec_type.empty()) {
    sec_type = "STK";
  }

  spec = {};
  spec.sec_type = sec_type;
  spec.con_id = con_id;
  spec.quantity_scale = 4;
  if (sec_type == "STK") {
    spec.canonical_symbol = base;
    spec.request_symbol = base;
    spec.exchange = "SMART";
    spec.primary_exchange = primary;
    spec.currency = quote.empty() ? "USD" : quote;
    spec.price_scale = 4;
    return valid_symbol(base) && spec.currency == "USD" && !primary.empty();
  }
  if (sec_type == "CASH") {
    spec.canonical_symbol = base;
    if (base.size() == 3 && quote.size() == 3) {
      spec.canonical_symbol += quote;
    }
    if (spec.canonical_symbol.size() != 6 || quote.size() != 3) {
      return false;
    }
    spec.request_symbol = spec.canonical_symbol.substr(0, 3);
    spec.currency = quote;
    spec.exchange = "IDEALPRO";
    spec.price_scale = std::nullopt;
    return valid_symbol(spec.canonical_symbol);
  }
  if (sec_type == "CMDTY") {
    spec.canonical_symbol = base;
    spec.request_symbol = base;
    spec.exchange = "SMART";
    spec.primary_exchange = primary;
    spec.currency = quote.empty() ? "USD" : quote;
    spec.price_scale = std::nullopt;
    return valid_symbol(base);
  }
  return false;
}

bool execute_command(PGconn *connection, const char *command,
                     ExecStatusType wanted, std::string &error) {
  Result result(PQexec(connection, command));
  if (result != nullptr && PQresultStatus(result.get()) == wanted) {
    return true;
  }
  error = PQerrorMessage(connection);
  return false;
}

}  // namespace

std::string_view postgres_candidate_query() noexcept {
  return kCandidateQuery;
}

std::string_view postgres_turnover_update_query() noexcept {
  return kTurnoverUpdateQuery;
}

bool load_postgres_candidates(std::string_view url_env,
                              CandidateLoad &loaded, std::string &error) {
  loaded = {};
  const std::string env_name(url_env);
  const char *url = std::getenv(env_name.c_str());
  if (url == nullptr || url[0] == '\0') {
    error = "ibkr postgres environment " + env_name + " is empty";
    return false;
  }

  Connection connection(PQconnectdb(url));
  if (connection == nullptr || PQstatus(connection.get()) != CONNECTION_OK) {
    error = "ibkr postgres connection failed: " +
            std::string(connection == nullptr
                            ? "null connection"
                            : PQerrorMessage(connection.get()));
    return false;
  }
  if (!execute_command(connection.get(), "BEGIN READ ONLY",
                       PGRES_COMMAND_OK, error)) {
    error = "ibkr postgres read-only transaction failed: " + error;
    return false;
  }

  Result result(PQexec(connection.get(), kCandidateQuery.data()));
  if (result == nullptr || PQresultStatus(result.get()) != PGRES_TUPLES_OK) {
    error = "ibkr postgres candidate query failed: " +
            std::string(PQerrorMessage(connection.get()));
    return false;
  }

  std::set<std::int64_t> con_ids;
  const int rows = PQntuples(result.get());
  loaded.subscriptions.reserve(static_cast<std::size_t>(rows));
  for (int row = 0; row < rows; ++row) {
    InstrumentSpec spec;
    if (!candidate_from_row(result.get(), row, spec) ||
        !con_ids.insert(spec.con_id).second) {
      ++loaded.skipped;
      continue;
    }
    loaded.subscriptions.push_back(std::move(spec));
  }
  std::string rollback_error;
  if (!execute_command(connection.get(), "ROLLBACK", PGRES_COMMAND_OK,
                       rollback_error)) {
    error = "ibkr postgres rollback failed: " + rollback_error;
    return false;
  }
  if (loaded.subscriptions.empty()) {
    error = "ibkr postgres returned no valid subscriptions";
    return false;
  }
  return true;
}

bool persist_postgres_turnover_metrics(
    std::string_view url_env,
    const std::vector<InstrumentTurnoverMetric> &metrics,
    std::size_t &updated, std::string &error) {
  updated = 0;
  error.clear();
  if (metrics.empty()) {
    return true;
  }
  const std::string env_name(url_env);
  const char *url = std::getenv(env_name.c_str());
  if (url == nullptr || url[0] == '\0') {
    error = "ibkr postgres environment " + env_name + " is empty";
    return false;
  }
  Connection connection(PQconnectdb(url));
  if (connection == nullptr || PQstatus(connection.get()) != CONNECTION_OK) {
    error = "ibkr postgres connection failed: " +
            std::string(connection == nullptr
                            ? "null connection"
                            : PQerrorMessage(connection.get()));
    return false;
  }
  if (!execute_command(connection.get(), "BEGIN", PGRES_COMMAND_OK, error)) {
    error = "ibkr postgres turnover transaction failed: " + error;
    return false;
  }
  const auto rollback = [&] {
    std::string ignored;
    (void)execute_command(connection.get(), "ROLLBACK", PGRES_COMMAND_OK,
                          ignored);
  };
  for (const auto &metric : metrics) {
    if (metric.con_id <= 0 || !std::isfinite(metric.turnover_usd_90d) ||
        metric.turnover_usd_90d < 0.0) {
      rollback();
      error = "ibkr postgres turnover metric is invalid for " +
              metric.canonical_symbol;
      return false;
    }
    const auto con_id = std::to_string(metric.con_id);
    std::ostringstream turnover;
    turnover << std::setprecision(17) << metric.turnover_usd_90d;
    const auto turnover_text = turnover.str();
    const char *values[] = {con_id.c_str(), turnover_text.c_str()};
    Result result(PQexecParams(connection.get(), kTurnoverUpdateQuery.data(), 2,
                               nullptr, values, nullptr, nullptr, 0));
    if (result == nullptr ||
        PQresultStatus(result.get()) != PGRES_COMMAND_OK) {
      error = "ibkr postgres turnover update failed for " +
              metric.canonical_symbol + ": " + PQerrorMessage(connection.get());
      rollback();
      return false;
    }
    const char *rows = PQcmdTuples(result.get());
    if (rows != nullptr && rows[0] != '\0') {
      updated += static_cast<std::size_t>(std::strtoull(rows, nullptr, 10));
    }
  }
  if (!execute_command(connection.get(), "COMMIT", PGRES_COMMAND_OK, error)) {
    error = "ibkr postgres turnover commit failed: " + error;
    rollback();
    updated = 0;
    return false;
  }
  return true;
}

}  // namespace mds::ibkr

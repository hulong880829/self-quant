#include "mds/ibkr/ibkr_runtime.h"

#include "mds/ibkr/ibkr_mailbox.h"
#include "mds/ibkr/ibkr_universe.h"
#include "mds/ibkr/ibkr_wake.h"
#include "mds/instrument/instrument_manager.h"
#include "mds/publish/wire_publisher.h"
#include "mds/transport/shared_ring.h"
#include "utils/md/decimal.h"
#include "utils/runtime/timestamp.h"

#include <sys/eventfd.h>
#include <fcntl.h>
#include <poll.h>
#include <unistd.h>

#include <algorithm>
#include <array>
#include <cctype>
#include <chrono>
#include <cmath>
#include <cstdlib>
#include <cstring>
#include <deque>
#include <exception>
#include <iostream>
#include <limits>
#include <memory>
#include <mutex>
#include <optional>
#include <random>
#include <string>
#include <thread>
#include <unordered_map>
#include <utility>
#include <vector>

namespace mds::ibkr {
namespace {

using Clock = std::chrono::steady_clock;

utils::md::IbkrSecurityType security_type_of(std::string_view text) noexcept {
  std::string upper(text);
  for (char &character : upper) {
    character = static_cast<char>(std::toupper(static_cast<unsigned char>(character)));
  }
  if (upper == "STK") return utils::md::IbkrSecurityType::Stock;
  if (upper == "CMDTY") return utils::md::IbkrSecurityType::Commodity;
  if (upper == "CASH") return utils::md::IbkrSecurityType::Cash;
  return utils::md::IbkrSecurityType::Unknown;
}

bool positive_decimal(std::string_view text, std::uint8_t scale,
                      std::int64_t &out) noexcept {
  std::uint8_t required = 0;
  if (text.empty() || !utils::md::decimal_scale(text, required) ||
      required > scale ||
      !utils::md::decimal_to_fixed(text, scale, out) || out <= 0) {
    return false;
  }
  return true;
}

bool multiplier_from_details(utils::md::IbkrSecurityType type,
                             std::string_view text, std::int64_t &mantissa,
                             std::uint8_t &scale) noexcept {
  if (text.empty() || text == "0") {
    if (type == utils::md::IbkrSecurityType::Commodity) {
      return false;
    }
    mantissa = 1;
    scale = 0;
    return type == utils::md::IbkrSecurityType::Stock ||
           type == utils::md::IbkrSecurityType::Cash;
  }
  if (!utils::md::decimal_scale(text, scale) ||
      !utils::md::decimal_to_fixed(text, scale, mantissa) || mantissa <= 0) {
    return false;
  }
  return true;
}

template <std::size_t Size>
void copy_text(std::array<char, Size> &destination, std::string_view source) {
  const auto length = std::min(source.size(), destination.size() - 1);
  std::memcpy(destination.data(), source.data(), length);
  destination[length] = '\0';
}

std::uint64_t sample_tsc() noexcept {
  return utils::runtime::Timestamp::NowTSC().cycles;
}

bool same_text(std::string_view left, std::string_view right) noexcept {
  if (left.size() != right.size()) {
    return false;
  }
  for (std::size_t index = 0; index < left.size(); ++index) {
    const auto lhs = static_cast<unsigned char>(left[index]);
    const auto rhs = static_cast<unsigned char>(right[index]);
    if (std::toupper(lhs) != std::toupper(rhs)) {
      return false;
    }
  }
  return true;
}

bool same_request(const InstrumentSpec &left, const InstrumentSpec &right) noexcept {
  return left.canonical_symbol == right.canonical_symbol &&
         left.request_symbol == right.request_symbol &&
         same_text(left.sec_type, right.sec_type) &&
         same_text(left.exchange, right.exchange) &&
         same_text(left.primary_exchange, right.primary_exchange) &&
         same_text(left.currency, right.currency);
}

bool contract_matches(const InstrumentSpec &spec,
                      const IbkrContractDetails &details) noexcept {
  if (details.con_id <= 0 || !same_text(details.currency, spec.currency)) {
    return false;
  }
  if (spec.con_id > 0 && details.con_id != spec.con_id) {
    return false;
  }
  const bool symbol_match = same_text(details.symbol, spec.request_symbol) ||
                            same_text(details.local_symbol, spec.request_symbol);
  if (!symbol_match) {
    return false;
  }
  if (same_text(spec.sec_type, "STK")) {
    return same_text(details.sec_type, "STK") &&
           same_text(details.primary_exchange, spec.primary_exchange);
  }
  if (same_text(spec.sec_type, "CASH")) {
    return same_text(details.sec_type, "CASH");
  }
  if (same_text(spec.sec_type, "CMDTY")) {
    return same_text(details.sec_type, "CMDTY") || same_text(details.sec_type, "CASH");
  }
  return false;
}

constexpr int kMetricTickerBase = 1'000'000;
constexpr int kContractReqIdBase = 2'000'000;
constexpr auto kHealthSummaryInterval = std::chrono::seconds(30);
constexpr auto kInformationalLogInterval = std::chrono::seconds(30);

const char *session_state_name(SessionState state) noexcept {
  switch (state) {
    case SessionState::Disconnected:
      return "disconnected";
    case SessionState::Connecting:
      return "connecting";
    case SessionState::Handshaking:
      return "handshaking";
    case SessionState::ResolvingContracts:
      return "resolving_contracts";
    case SessionState::ProbingMetrics:
      return "probing_metrics";
    case SessionState::Subscribing:
      return "subscribing";
    case SessionState::Live:
      return "live";
    case SessionState::Reconnecting:
      return "reconnecting";
  }
  return "unknown";
}

const char *error_class_name(IbkrErrorClass klass) noexcept {
  switch (klass) {
    case IbkrErrorClass::Ignored:
      return "ignored";
    case IbkrErrorClass::ConnectionLost:
      return "connection_lost";
    case IbkrErrorClass::ConnectionRestoredDataLost:
      return "connection_restored_data_lost";
    case IbkrErrorClass::ConnectionRestoredDataMaintained:
      return "connection_restored_data_maintained";
    case IbkrErrorClass::Contract:
      return "contract";
    case IbkrErrorClass::MarketDataDenied:
      return "market_data";
    case IbkrErrorClass::Informational:
      return "informational";
  }
  return "unknown";
}

bool market_data_denied(int code) noexcept {
  switch (code) {
    case 322:
    case 354:
    case 10089:
    case 10090:
    case 10167:
    case 10168:
    case 10190:
    case 10197:
      return true;
    default:
      return false;
  }
}

bool metric_index(int ticker_id, std::size_t count,
                  std::size_t &index) noexcept {
  if (ticker_id < kMetricTickerBase) {
    return false;
  }
  index = static_cast<std::size_t>(ticker_id - kMetricTickerBase);
  return index < count;
}

std::optional<double> parse_nonnegative(std::string_view text) noexcept {
  if (text.empty()) {
    return std::nullopt;
  }
  const std::string copy(text);
  char *end = nullptr;
  const double value = std::strtod(copy.c_str(), &end);
  if (end == copy.c_str() || *end != '\0' || !std::isfinite(value) ||
      value < 0.0) {
    return std::nullopt;
  }
  return value;
}

std::optional<double> parse_market_cap(std::string_view ratios) noexcept {
  constexpr std::string_view key = "MKTCAP=";
  std::size_t begin = 0;
  while (begin < ratios.size()) {
    const auto end = ratios.find(';', begin);
    const auto token = ratios.substr(
        begin, end == std::string_view::npos ? ratios.size() - begin
                                             : end - begin);
    if (token.starts_with(key)) {
      const auto raw = parse_nonnegative(token.substr(key.size()));
      if (!raw.has_value()) {
        return std::nullopt;
      }
      // IB's Reuters fundamental-ratio feed reports MKTCAP in millions.
      return *raw * 1'000'000.0;
    }
    if (end == std::string_view::npos) {
      break;
    }
    begin = end + 1;
  }
  return std::nullopt;
}

int reference_price_priority(ReferencePriceSource source) noexcept {
  return static_cast<int>(source);
}

const char *reference_price_source_name(ReferencePriceSource source) noexcept {
  switch (source) {
    case ReferencePriceSource::DelayedLast:
      return "delayed_last";
    case ReferencePriceSource::Last:
      return "last";
    case ReferencePriceSource::DelayedClose:
      return "delayed_close";
    case ReferencePriceSource::Close:
      return "close";
    case ReferencePriceSource::None:
      return "none";
  }
  return "none";
}

}  // namespace

struct IbkrRuntime::Impl {
  struct Collection {
    std::mutex mutex;
    std::vector<IbkrContractDetails> rows;
    bool ended{false};
  };

  struct Slot {
    InstrumentSpec spec;
    utils::md::InstrumentId instrument_id{};
    std::optional<BboAssembler> assembler;
    std::uint8_t resolved_price_scale{0};
    std::int64_t tick_size{0};
    std::int64_t lot_size{0};
    std::int64_t contract_multiplier{0};
    std::uint8_t contract_multiplier_scale{0};
    utils::md::IbkrSecurityType security_type{utils::md::IbkrSecurityType::Unknown};
    SeqlockMailbox mailbox;
    std::atomic<bool> dirty{false};
    std::atomic<int> contract_state{0};
    std::atomic<bool> isolated{false};
    std::atomic<bool> suppressed{false};
    std::atomic<bool> catalog_ok{false};
    std::atomic<bool> ready{false};
    std::atomic<bool> market_active{false};
    bool selected{false};
    std::uint64_t source_seq{0};
    IbkrContractDetails details{};
    bool accepted{false};
    std::mutex metric_mutex;
    SubscriptionMetrics metrics;
    bool average_volume_received{false};
    bool market_cap_received{false};
    std::optional<double> reference_price_usd;
    ReferencePriceSource reference_price_source{ReferencePriceSource::None};

    explicit Slot(InstrumentSpec specification) : spec(std::move(specification)) {}
  };

  explicit Impl(RuntimeOptions runtime_options)
      : options(std::move(runtime_options)) {
    next_contract_req_id = options.contract_req_id_origin;
  }

  RuntimeOptions options;
  std::deque<Slot> slots;
  std::deque<std::unique_ptr<Collection>> collections;
  instrument::InstrumentManager identities;
  enum class ResolveStatus : std::uint8_t {
    Complete = 0,
    RetrySession,
    Fatal,
  };

  struct PendingContract {
    std::size_t slot_index{};
    std::uint64_t session_generation{};
    Clock::time_point started_at{};
  };

  std::atomic<SessionState> state{SessionState::Disconnected};
  std::atomic<std::uint32_t> generation{1};
  std::atomic<bool> stopping{false};
  std::atomic<bool> connected{false};
  std::atomic<bool> session_retry_requested{false};
  std::atomic<bool> terminal_failure{false};
  std::atomic<int> process_exit_code{0};
  std::atomic<bool> publisher_ready{false};
  std::atomic<std::uint64_t> publish_count{0};
  std::atomic<std::uint64_t> overwrite_count{0};
  std::atomic<std::uint64_t> catalog_failures{0};
  std::atomic<std::uint64_t> catalog_publish_count{0};
  std::atomic<std::uint64_t> active_subscriptions{0};
  std::atomic<std::uint64_t> session_generation{0};
  std::atomic<std::uint64_t> stale_contract_callbacks{0};
  std::atomic<std::uint64_t> informational_errors{0};
  std::atomic<std::uint64_t> metric_success_average_volume{0};
  std::atomic<std::uint64_t> metric_success_market_cap{0};
  std::atomic<std::uint64_t> metric_success_reference_price{0};
  std::atomic<std::uint64_t> metric_missing_reference_price{0};
  std::atomic<std::uint64_t> metric_persisted_turnover{0};
  std::atomic<std::uint64_t> filtered_below_threshold{0};
  std::atomic<std::uint64_t> missing_metric_rejected{0};
  std::atomic<std::uint64_t> non_stock_exempt{0};
  std::atomic<std::uint64_t> truncated{0};
  std::atomic<std::uint64_t> selected_non_stock{0};
  std::atomic<std::uint64_t> selected_stock{0};
  std::atomic<std::uint64_t> selected_subscriptions{0};
  bool universe_selected{false};
  bool market_cap_filter_degraded{false};
  bool health_emitted{false};
  bool ready_emitted{false};
  bool first_bbo_emitted{false};
  bool last_ready{false};
  SessionState last_health_state{SessionState::Disconnected};
  std::uint64_t last_selected{0};
  std::uint64_t last_active{0};
  std::uint64_t last_catalogs{0};
  std::uint64_t last_health_generation{0};
  Clock::time_point last_health_emit{};
  int next_contract_req_id{kContractReqIdBase};
  std::mutex pending_mu;
  std::mutex failure_mu;
  std::mutex health_mu;
  std::mutex info_mu;
  std::string terminal_error{"ibkr producer failed"};
  std::string retry_reason;
  std::unordered_map<int, PendingContract> pending_contracts;
  std::unordered_map<int, std::pair<std::string, Clock::time_point>>
      informational_logs;
  int event_fd{-1};
  WakeGate wake;
  std::unique_ptr<publish::WirePublisher> publisher;
  std::thread publisher_thread;
  std::thread control_thread;
  int backoff_ms{0};

  void publish_catalog(Slot &slot) noexcept {
    if (publisher == nullptr) {
      return;
    }
    utils::md::InstrumentCatalog catalog{};
    catalog.instrument_id = slot.instrument_id;
    catalog.venue = utils::md::Venue::Ibkr;
    catalog.product_type = utils::md::ProductType::Equity;
    catalog.price_scale = slot.resolved_price_scale;
    catalog.quantity_scale = slot.spec.quantity_scale;
    copy_text(catalog.canonical_symbol, slot.spec.canonical_symbol);
    copy_text(catalog.venue_symbol, std::to_string(slot.details.con_id));
    const auto base = slot.details.symbol.empty() ? slot.spec.request_symbol
                                                  : slot.details.symbol;
    const auto quote = slot.details.currency.empty() ? slot.spec.currency
                                                     : slot.details.currency;
    copy_text(catalog.base_asset, base);
    copy_text(catalog.quote_asset, quote);
    copy_text(catalog.settle_asset, quote);
    catalog.tick_size = slot.tick_size;
    catalog.lot_size = slot.lot_size;
    catalog.contract_multiplier = slot.contract_multiplier;
    catalog.contract_multiplier_scale = slot.contract_multiplier_scale;
    catalog.ibkr_security_type = slot.security_type;
    utils::md::EventHeader header{};
    header.instrument_id = slot.instrument_id;
    header.book_generation = generation.load(std::memory_order_acquire);
    header.exchange_ts_ns = 0;
    header.receive_tsc = sample_tsc();
    header.publish_tsc = header.receive_tsc;
    header.state = utils::md::BookState::Live;
    header.bbo_origin = utils::md::BboOrigin::TickerStream;
    const auto published = publisher->publish_instrument_catalog(header, catalog);
    slot.catalog_ok.store(static_cast<bool>(published), std::memory_order_release);
    if (!published) {
      catalog_failures.fetch_add(1, std::memory_order_relaxed);
      return;
    }
    catalog_publish_count.fetch_add(1, std::memory_order_relaxed);
  }

  void publish_all_catalogs(bool force) noexcept {
    bool published = false;
    for (auto &slot : slots) {
      if (!slot.ready.load(std::memory_order_acquire) ||
          slot.isolated.load(std::memory_order_acquire)) {
        continue;
      }
      if (!force && slot.catalog_ok.load(std::memory_order_acquire)) {
        continue;
      }
      const auto before = catalog_publish_count.load(std::memory_order_relaxed);
      publish_catalog(slot);
      published = published ||
                  catalog_publish_count.load(std::memory_order_relaxed) != before;
    }
    if (published) {
      note_health(force ? "catalog_republish" : nullptr);
    }
  }

  void invalidate_books() noexcept {
    BboSnapshot empty{};
    empty.book_generation = generation.load(std::memory_order_acquire);
    empty.publishable = false;
    for (auto &slot : slots) {
      slot.mailbox.write(empty);
      slot.dirty.store(false, std::memory_order_release);
    }
  }

  void advance_generation_and_clear_books() noexcept {
    const auto next = generation.fetch_add(1, std::memory_order_acq_rel) + 1;
    BboSnapshot empty{};
    empty.book_generation = next;
    empty.publishable = false;
    for (auto &slot : slots) {
      slot.mailbox.write(empty);
      slot.dirty.store(false, std::memory_order_release);
    }
  }

  void clear_market_active() noexcept {
    for (auto &slot : slots) {
      slot.market_active.store(false, std::memory_order_release);
    }
    active_subscriptions.store(0, std::memory_order_release);
  }

  void activate_market_slot(Slot &slot) noexcept {
    if (!slot.market_active.exchange(true, std::memory_order_acq_rel)) {
      active_subscriptions.fetch_add(1, std::memory_order_relaxed);
    }
  }

  void deactivate_market_slot(Slot &slot) noexcept {
    if (slot.market_active.exchange(false, std::memory_order_acq_rel)) {
      active_subscriptions.fetch_sub(1, std::memory_order_relaxed);
    }
  }

  [[nodiscard]] bool phase_aborted() const noexcept {
    return stopping.load(std::memory_order_acquire) ||
           session_retry_requested.load(std::memory_order_acquire) ||
           terminal_failure.load(std::memory_order_acquire);
  }

  void request_session_retry(const char *reason) noexcept {
    if (terminal_failure.load(std::memory_order_acquire) || reason == nullptr) {
      return;
    }
    if (session_retry_requested.exchange(true, std::memory_order_acq_rel)) {
      return;
    }
    {
      std::lock_guard lock(failure_mu);
      retry_reason = reason;
    }
    std::cerr << "ibkr session retry reason=" << reason
              << " session_generation="
              << session_generation.load(std::memory_order_relaxed) << '\n';
  }

  void fail_terminal(const char *reason) noexcept {
    if (reason == nullptr) {
      reason = "ibkr producer failed";
    }
    bool already = false;
    {
      std::lock_guard lock(failure_mu);
      already = terminal_failure.exchange(true, std::memory_order_acq_rel);
      if (!already) {
        terminal_error = reason;
        process_exit_code.store(1, std::memory_order_release);
      }
    }
    if (!already) {
      std::cerr << "ibkr fatal " << reason << '\n';
    }
  }

  void clear_pending_contracts() noexcept {
    std::lock_guard lock(pending_mu);
    pending_contracts.clear();
  }

  [[nodiscard]] bool begin_contract_request(std::size_t index, int &req_id,
                                            const char *&fatal) noexcept {
    std::lock_guard lock(pending_mu);
    if (next_contract_req_id < kContractReqIdBase ||
        next_contract_req_id >= std::numeric_limits<int>::max()) {
      fatal = "contract reqId exhausted";
      return false;
    }
    req_id = next_contract_req_id++;
    pending_contracts.insert_or_assign(
        req_id, PendingContract{index, session_generation.load(
                                            std::memory_order_relaxed),
                                Clock::now()});
    fatal = nullptr;
    return true;
  }

  enum class PendingMatch : std::uint8_t { Missing = 0, Stale, Current };

  [[nodiscard]] PendingMatch match_pending_contract(int req_id,
                                                    std::size_t &index,
                                                    bool erase,
                                                    bool count_missing) noexcept {
    std::lock_guard lock(pending_mu);
    const auto found = pending_contracts.find(req_id);
    if (found == pending_contracts.end()) {
      if (count_missing) {
        stale_contract_callbacks.fetch_add(1, std::memory_order_relaxed);
      }
      return PendingMatch::Missing;
    }
    if (found->second.session_generation !=
        session_generation.load(std::memory_order_relaxed)) {
      stale_contract_callbacks.fetch_add(1, std::memory_order_relaxed);
      if (erase) {
        pending_contracts.erase(found);
      }
      return PendingMatch::Stale;
    }
    index = found->second.slot_index;
    if (erase) {
      pending_contracts.erase(found);
    }
    if (index >= slots.size()) {
      return PendingMatch::Stale;
    }
    return PendingMatch::Current;
  }

  void note_health(const char *force_reason) noexcept {
    std::lock_guard lock(health_mu);
    const auto selected = selected_subscriptions.load(std::memory_order_relaxed);
    const auto active = active_subscriptions.load(std::memory_order_relaxed);
    const auto catalogs = catalog_publish_count.load(std::memory_order_relaxed);
    const auto bbo = publish_count.load(std::memory_order_relaxed);
    const auto generation_now =
        session_generation.load(std::memory_order_relaxed);
    const auto current = state.load(std::memory_order_acquire);
    const bool ready = selected > 0 && active > 0 && catalogs > 0 &&
                       !terminal_failure.load(std::memory_order_acquire);
    const auto now = Clock::now();
    const bool summary_due =
        health_emitted && now - last_health_emit >= kHealthSummaryInterval;
    const bool changed = !health_emitted || ready != last_ready ||
                         selected != last_selected || active != last_active ||
                         catalogs != last_catalogs ||
                         generation_now != last_health_generation ||
                         current != last_health_state;
    const bool first_ready = ready && !ready_emitted;
    const bool forced = force_reason != nullptr;
    if (!changed && !summary_due && !forced && !first_ready) {
      return;
    }
    std::cerr << "ibkr health ready=" << (ready ? "true" : "false")
              << " selected=" << selected << " active=" << active
              << " catalogs=" << catalogs << " bbo=" << bbo
              << " session_generation=" << generation_now
              << " session_state=" << session_state_name(current);
    if (!ready) {
      const char *why = "not_ready";
      if (terminal_failure.load(std::memory_order_acquire)) {
        why = "fatal";
      } else if (selected == 0) {
        why = "selected=0";
      } else if (active == 0) {
        why = "active=0";
      } else if (catalogs == 0) {
        why = "catalogs=0";
      }
      std::cerr << " reason=" << why;
    }
    std::cerr << '\n';
    health_emitted = true;
    ready_emitted = ready_emitted || ready;
    last_ready = ready;
    last_selected = selected;
    last_active = active;
    last_catalogs = catalogs;
    last_health_generation = generation_now;
    last_health_state = current;
    last_health_emit = now;
  }

  void set_state(SessionState next) noexcept {
    state.store(next, std::memory_order_release);
    note_health(nullptr);
  }

  void note_informational(int code, std::string_view message) noexcept {
    informational_errors.fetch_add(1, std::memory_order_relaxed);
    const auto now = Clock::now();
    std::lock_guard lock(info_mu);
    auto &entry = informational_logs[code];
    entry.first.assign(message.begin(), message.end());
    if (entry.second != Clock::time_point{} &&
        now - entry.second < kInformationalLogInterval) {
      return;
    }
    entry.second = now;
    std::cerr << "ibkr error id=-1 code=" << code << " message=\""
              << entry.first << "\" class=informational session_state="
              << session_state_name(state.load(std::memory_order_acquire))
              << " session_generation="
              << session_generation.load(std::memory_order_relaxed)
              << " request_kind=session\n";
  }

  bool wait_interval(std::chrono::milliseconds duration) {
    auto left = duration;
    while (left.count() > 0 && !phase_aborted()) {
      const auto slice = std::min(left, std::chrono::milliseconds(20));
      std::this_thread::sleep_for(slice);
      left -= slice;
    }
    return !phase_aborted();
  }

  void note_quote(Slot &slot, const AssemblerUpdate &update,
                  std::uint64_t receive_tsc) noexcept {
    if (!update.changed) {
      return;
    }
    const auto quote = slot.assembler->quote();
    BboSnapshot snapshot{};
    snapshot.bid_price = quote.bid_price;
    snapshot.ask_price = quote.ask_price;
    snapshot.bid_size = quote.bid_size;
    snapshot.ask_size = quote.ask_size;
    snapshot.book_generation =
        static_cast<std::uint32_t>(slot.assembler->generation());
    snapshot.receive_tsc = receive_tsc == 0 ? sample_tsc() : receive_tsc;
    snapshot.publishable = update.publishable &&
                           !slot.suppressed.load(std::memory_order_acquire) &&
                           !slot.isolated.load(std::memory_order_acquire);
    slot.mailbox.write(snapshot);
    mark_dirty(slot.dirty, overwrite_count);
    (void)wake.arm();
  }

  Slot *slot_for(int id) noexcept {
    if (id <= 0 || static_cast<std::size_t>(id) > slots.size()) {
      return nullptr;
    }
    return &slots[static_cast<std::size_t>(id - 1)];
  }

  bool quotes_accepted() const noexcept {
    const auto current = state.load(std::memory_order_acquire);
    return current == SessionState::Subscribing || current == SessionState::Live;
  }

  void add_subscription(const InstrumentSpec &spec) {
    slots.emplace_back(spec);
    collections.push_back(std::make_unique<Collection>());
  }

  void isolate_slot(Slot &slot, const char *reason) noexcept {
    std::cerr << "ibkr isolate " << slot.spec.canonical_symbol << " " << reason
              << '\n';
    slot.accepted = false;
    slot.isolated.store(true, std::memory_order_release);
    slot.contract_state.store(2, std::memory_order_release);
  }

  void reset_collection(std::size_t index) noexcept {
    auto &collection = *collections[index];
    std::lock_guard lock(collection.mutex);
    collection.rows.clear();
    collection.ended = false;
  }

  [[nodiscard]] bool collection_finished(std::size_t index) noexcept {
    if (slots[index].contract_state.load(std::memory_order_acquire) == 2) {
      return true;
    }
    auto &collection = *collections[index];
    std::lock_guard lock(collection.mutex);
    return collection.ended;
  }

  void decide_slot(std::size_t index, bool recheck) noexcept {
    auto &slot = slots[index];
    if (slot.contract_state.load(std::memory_order_acquire) == 2 &&
        slot.isolated.load(std::memory_order_acquire)) {
      return;
    }
    std::vector<IbkrContractDetails> rows;
    {
      auto &collection = *collections[index];
      std::lock_guard lock(collection.mutex);
      if (!collection.ended) {
        return;
      }
      rows = collection.rows;
    }
    std::vector<IbkrContractDetails> unique;
    for (const auto &row : rows) {
      if (!contract_matches(slot.spec, row)) {
        continue;
      }
      const auto known = std::find_if(
          unique.begin(), unique.end(),
          [&](const IbkrContractDetails &existing) {
            return existing.con_id == row.con_id;
          });
      if (known == unique.end()) {
        unique.push_back(row);
      }
    }
    if (unique.empty()) {
      isolate_slot(slot, "contract not found");
      return;
    }
    if (unique.size() != 1) {
      isolate_slot(slot, "ambiguous");
      return;
    }
    const auto &chosen = unique.front();
    if (recheck) {
      if (chosen.con_id != slot.details.con_id ||
          !same_text(chosen.sec_type, slot.details.sec_type) ||
          !same_text(chosen.currency, slot.details.currency)) {
        isolate_slot(slot, "identity conflict");
        return;
      }
      slot.contract_state.store(1, std::memory_order_release);
      return;
    }
    std::uint8_t scale = 0;
    if (slot.spec.price_scale.has_value()) {
      scale = *slot.spec.price_scale;
      if (!min_tick_fits_scale(chosen.min_tick, scale)) {
        isolate_slot(slot, "minTick does not fit price_scale");
        return;
      }
    } else if (!price_scale_for_tick(chosen.min_tick, scale)) {
      isolate_slot(slot, "minTick scale");
      return;
    }
    const auto security = security_type_of(chosen.sec_type);
    std::int64_t tick_units = 0;
    std::int64_t lot = 0;
    std::int64_t minimum_size = 0;
    std::int64_t multiplier = 0;
    std::uint8_t multiplier_scale = 0;
    if (security == utils::md::IbkrSecurityType::Unknown ||
        !min_tick_fits_scale(chosen.min_tick, scale) ||
        !double_price_to_fixed(chosen.min_tick, scale, tick_units) ||
        !positive_decimal(chosen.size_increment_text, slot.spec.quantity_scale,
                          lot) ||
        !positive_decimal(chosen.min_size_text, slot.spec.quantity_scale,
                          minimum_size) ||
        !multiplier_from_details(security, chosen.multiplier_text, multiplier,
                                 multiplier_scale)) {
      isolate_slot(slot, "execution metadata");
      return;
    }
    (void)minimum_size;
    slot.details = chosen;
    slot.resolved_price_scale = scale;
    slot.tick_size = tick_units;
    slot.lot_size = lot;
    slot.contract_multiplier = multiplier;
    slot.contract_multiplier_scale = multiplier_scale;
    slot.security_type = security;
    slot.assembler.emplace(scale, slot.spec.quantity_scale);
    if (slot.instrument_id == 0) {
      std::string identity_error;
      if (!identities.resolve_instance(
              utils::md::Venue::Ibkr, utils::md::ProductType::Equity,
              slot.spec.canonical_symbol, std::to_string(chosen.con_id),
              slot.instrument_id, identity_error)) {
        std::cerr << "ibkr identity " << identity_error << '\n';
        isolate_slot(slot, "identity");
        return;
      }
    }
    slot.accepted = true;
    slot.contract_state.store(1, std::memory_order_release);
  }

  void settle_identities() noexcept {
    std::unordered_map<std::int64_t, std::vector<std::size_t>> groups;
    for (std::size_t index = 0; index < slots.size(); ++index) {
      if (slots[index].accepted &&
          !slots[index].isolated.load(std::memory_order_acquire)) {
        groups[slots[index].details.con_id].push_back(index);
      }
    }
    for (const auto &[con_id, indexes] : groups) {
      (void)con_id;
      if (indexes.size() < 2) {
        continue;
      }
      bool same = true;
      for (std::size_t offset = 1; offset < indexes.size(); ++offset) {
        if (!same_request(slots[indexes[0]].spec, slots[indexes[offset]].spec)) {
          same = false;
          break;
        }
      }
      if (same) {
        for (std::size_t offset = 1; offset < indexes.size(); ++offset) {
          isolate_slot(slots[indexes[offset]], "duplicate");
        }
      } else {
        for (const auto index : indexes) {
          isolate_slot(slots[index], "identity conflict");
        }
      }
    }
  }

  [[nodiscard]] bool metric_complete(
      std::size_t index, bool need_average_volume,
      bool need_market_cap, bool allow_last_fallback = false) noexcept {
    auto &slot = slots[index];
    std::lock_guard lock(slot.metric_mutex);
    const bool reference_ready =
        slot.reference_price_usd.has_value() &&
        (allow_last_fallback ||
         slot.reference_price_source == ReferencePriceSource::Close ||
         slot.reference_price_source == ReferencePriceSource::DelayedClose);
    return (!need_average_volume || slot.average_volume_received) &&
           (!need_market_cap || slot.market_cap_received) &&
           reference_ready;
  }

  void reset_metrics(std::size_t index) noexcept {
    auto &slot = slots[index];
    std::lock_guard lock(slot.metric_mutex);
    slot.metrics = {};
    slot.average_volume_received = false;
    slot.market_cap_received = false;
    slot.reference_price_usd.reset();
    slot.reference_price_source = ReferencePriceSource::None;
  }

  [[nodiscard]] bool probe_metrics(IbkrClient &client) noexcept {
    market_cap_filter_degraded = false;
    struct ActiveProbe {
      std::size_t index{};
      Clock::time_point deadline;
    };
    std::deque<std::size_t> pending;
    for (std::size_t index = 0; index < slots.size(); ++index) {
      auto &slot = slots[index];
      if (!slot.accepted || slot.isolated.load(std::memory_order_acquire) ||
          !same_text(slot.details.sec_type, "STK")) {
        continue;
      }
      reset_metrics(index);
      pending.push_back(index);
    }
    if (pending.empty()) {
      return true;
    }

    const auto probe_timeout =
        std::chrono::milliseconds(kMetricProbeTimeoutMs);
    const auto request_gap = std::chrono::milliseconds(
        1000 / std::max(options.subscribe_pacing_per_second, 1));
    const auto stopped = [&] { return phase_aborted(); };
    const auto launch_probe = [&](std::size_t index,
                                  std::string_view generic_ticks,
                                  std::vector<ActiveProbe> &active) {
      std::string request_error;
      const int ticker_id = kMetricTickerBase + static_cast<int>(index);
      if (!client.request_market_data(ticker_id, contract_request(slots[index], true),
                                      generic_ticks, request_error)) {
        std::cerr << "ibkr metric probe request failed "
                  << slots[index].spec.canonical_symbol << ": "
                  << request_error << '\n';
        return false;
      }
      active.push_back({index, Clock::now() + probe_timeout});
      return true;
    };
    const auto has_market_cap = [&](std::size_t index) {
      auto &slot = slots[index];
      std::lock_guard lock(slot.metric_mutex);
      return slot.metrics.market_cap_usd.has_value();
    };

    bool request_market_cap = options.filter.min_market_cap_usd.has_value();
    if (request_market_cap) {
      std::vector<ActiveProbe> canaries;
      canaries.reserve(kMarketCapCanaryCount);
      while (!pending.empty() && canaries.size() < kMarketCapCanaryCount &&
             !stopped()) {
        const auto index = pending.front();
        pending.pop_front();
        (void)launch_probe(index, "165,258", canaries);
      }
      bool available = false;
      while (!canaries.empty() && !stopped()) {
        for (const auto &canary : canaries) {
          if (has_market_cap(canary.index)) {
            available = true;
          }
        }
        bool finished = true;
        const auto now = Clock::now();
        for (const auto &canary : canaries) {
          auto &slot = slots[canary.index];
          std::lock_guard lock(slot.metric_mutex);
          if (!slot.market_cap_received && now < canary.deadline) {
            finished = false;
          }
        }
        if (available || finished) {
          break;
        }
        client.pump(std::chrono::milliseconds(10));
      }
      for (const auto &canary : canaries) {
        client.cancel_market_data(
            kMetricTickerBase + static_cast<int>(canary.index));
        if (!available ||
            !metric_complete(canary.index, true, true)) {
          if (!available &&
              metric_complete(canary.index, true, false, true)) {
            continue;
          }
          pending.push_front(canary.index);
        }
      }
      if (!available) {
        market_cap_filter_degraded = true;
        request_market_cap = false;
        std::cerr
            << "ibkr warning: market-cap canaries returned no MKTCAP; "
               "ignoring min_market_cap_usd for this startup\n";
      }
      if (stopped()) {
        return false;
      }
    }

    const std::string generic_ticks = request_market_cap ? "165,258" : "165";
    std::vector<ActiveProbe> active;
    active.reserve(kMetricProbeConcurrency);
    auto next_launch = Clock::now();
    while ((!pending.empty() || !active.empty()) && !stopped()) {
      const auto before_pump = Clock::now();
      if (!pending.empty() && active.size() < kMetricProbeConcurrency &&
          before_pump >= next_launch) {
        const auto index = pending.front();
        pending.pop_front();
        (void)launch_probe(index, generic_ticks, active);
        next_launch = before_pump + request_gap;
      }
      client.pump(std::chrono::milliseconds(10));
      const auto now = Clock::now();
      for (std::size_t offset = 0; offset < active.size();) {
        const auto index = active[offset].index;
        if (!metric_complete(index, true, request_market_cap) &&
            now < active[offset].deadline) {
          ++offset;
          continue;
        }
        client.cancel_market_data(kMetricTickerBase + static_cast<int>(index));
        active.erase(active.begin() + static_cast<std::ptrdiff_t>(offset));
      }
    }
    for (const auto &probe : active) {
      client.cancel_market_data(
          kMetricTickerBase + static_cast<int>(probe.index));
    }
    // Leave no application-side probe request active before persistent BBOs.
    active.clear();
    client.pump(std::chrono::milliseconds(50));
    return !stopped();
  }

  void persist_turnover_metrics() noexcept {
    std::vector<InstrumentTurnoverMetric> records;
    std::size_t missing_volume = 0;
    std::size_t missing_price = 0;
    std::size_t close_prices = 0;
    std::size_t last_prices = 0;
    for (auto &slot : slots) {
      if (!slot.accepted || slot.isolated.load(std::memory_order_acquire) ||
          !same_text(slot.details.sec_type, "STK")) {
        continue;
      }
      std::lock_guard lock(slot.metric_mutex);
      const bool has_volume = slot.metrics.average_volume_90d.has_value();
      const bool has_price = slot.reference_price_usd.has_value();
      if (!has_volume) {
        ++missing_volume;
      }
      if (!has_price) {
        ++missing_price;
      }
      if (!has_volume || !has_price) {
        continue;
      }
      const double turnover =
          *slot.metrics.average_volume_90d * *slot.reference_price_usd;
      if (!std::isfinite(turnover) || turnover < 0.0) {
        ++missing_price;
        continue;
      }
      if (slot.reference_price_source == ReferencePriceSource::Close ||
          slot.reference_price_source == ReferencePriceSource::DelayedClose) {
        ++close_prices;
      } else {
        ++last_prices;
      }
      std::cerr << "ibkr turnover metric symbol="
                << slot.spec.canonical_symbol
                << " con_id=" << slot.details.con_id
                << " average_volume_90d_shares="
                << *slot.metrics.average_volume_90d
                << " reference_price_usd=" << *slot.reference_price_usd
                << " price_source="
                << reference_price_source_name(slot.reference_price_source)
                << " turnover_usd_90d=" << turnover << '\n';
      records.push_back(
          {slot.details.con_id, slot.spec.canonical_symbol, turnover});
    }
    metric_success_reference_price.store(close_prices + last_prices,
                                         std::memory_order_relaxed);
    metric_missing_reference_price.store(missing_price,
                                         std::memory_order_relaxed);
    std::cerr << "ibkr turnover metrics complete=" << records.size()
              << " missing_volume=" << missing_volume
              << " missing_price=" << missing_price
              << " close_prices=" << close_prices
              << " last_prices=" << last_prices << '\n';
    if (!options.persist_turnover_metrics || records.empty()) {
      return;
    }
    std::string error;
    bool persisted = false;
    std::size_t updated = 0;
    try {
      persisted = options.persist_turnover_metrics(records, updated, error);
    } catch (const std::exception &caught) {
      error = caught.what();
    } catch (...) {
      error = "unknown exception";
    }
    if (!persisted) {
      std::cerr << "ibkr turnover persistence warning records="
                << records.size() << " error=\"" << error << "\"\n";
      return;
    }
    metric_persisted_turnover.store(updated, std::memory_order_relaxed);
    std::cerr << "ibkr turnover persistence requested=" << records.size()
              << " updated=" << updated << '\n';
  }

  void select_universe() noexcept {
    std::vector<InstrumentSpec> candidates;
    std::vector<SubscriptionMetrics> metrics;
    std::vector<std::size_t> slot_indexes;
    for (std::size_t index = 0; index < slots.size(); ++index) {
      auto &slot = slots[index];
      if (!slot.accepted || slot.isolated.load(std::memory_order_acquire)) {
        continue;
      }
      candidates.push_back(slot.spec);
      {
        std::lock_guard lock(slot.metric_mutex);
        metrics.push_back(slot.metrics);
      }
      slot_indexes.push_back(index);
    }

    auto filter = options.filter;
    if (market_cap_filter_degraded) {
      filter.min_market_cap_usd.reset();
    }
    const auto selection = select_subscriptions(candidates, metrics, filter);
    for (auto &slot : slots) {
      slot.selected = false;
      slot.ready.store(false, std::memory_order_release);
    }
    for (const auto candidate_index : selection.indexes) {
      auto &slot = slots[slot_indexes[candidate_index]];
      slot.selected = true;
      slot.ready.store(true, std::memory_order_release);
    }
    std::size_t average_count = 0;
    std::size_t market_cap_count = 0;
    for (const auto &value : metrics) {
      average_count += value.average_volume_90d.has_value() ? 1U : 0U;
      market_cap_count += value.market_cap_usd.has_value() ? 1U : 0U;
    }
    metric_success_average_volume.store(average_count,
                                        std::memory_order_relaxed);
    metric_success_market_cap.store(market_cap_count,
                                    std::memory_order_relaxed);
    filtered_below_threshold.store(selection.below_threshold,
                                   std::memory_order_relaxed);
    missing_metric_rejected.store(selection.missing_metric_rejected,
                                  std::memory_order_relaxed);
    non_stock_exempt.store(selection.non_stock_exempt,
                           std::memory_order_relaxed);
    truncated.store(selection.truncated, std::memory_order_relaxed);
    selected_non_stock.store(selection.selected_non_stock,
                             std::memory_order_relaxed);
    selected_stock.store(selection.selected_stock, std::memory_order_relaxed);
    selected_subscriptions.store(selection.indexes.size(),
                                 std::memory_order_relaxed);
    std::cerr << "ibkr universe candidates=" << candidates.size()
              << " non_stock_exempt=" << selection.non_stock_exempt
              << " avg_volume_values=" << average_count
              << " market_cap_values=" << market_cap_count
              << " below_threshold=" << selection.below_threshold
              << " missing_rejected=" << selection.missing_metric_rejected
              << " market_cap_filter_degraded="
              << (market_cap_filter_degraded ? "true" : "false")
              << " truncated=" << selection.truncated
              << " selected_non_stock=" << selection.selected_non_stock
              << " selected_stock=" << selection.selected_stock
              << " selected=" << selection.indexes.size() << '\n';
    universe_selected = true;
    note_health(nullptr);
  }

  IbkrContractRequest contract_request(const Slot &slot, bool resolved) const {
    IbkrContractRequest request;
    if (resolved) {
      request.con_id = slot.details.con_id;
      request.symbol = slot.details.symbol.empty() ? slot.spec.request_symbol
                                                   : slot.details.symbol;
      request.sec_type = slot.details.sec_type.empty() ? slot.spec.sec_type
                                                       : slot.details.sec_type;
      request.exchange = slot.details.exchange.empty() ? slot.spec.exchange
                                                       : slot.details.exchange;
      request.primary_exchange = slot.details.primary_exchange;
      request.currency = slot.details.currency.empty() ? slot.spec.currency
                                                       : slot.details.currency;
      return request;
    }
    request.con_id = slot.spec.con_id;
    request.symbol = slot.spec.request_symbol;
    request.sec_type = slot.spec.sec_type;
    request.exchange = slot.spec.exchange;
    request.primary_exchange = slot.spec.primary_exchange;
    request.currency = slot.spec.currency;
    return request;
  }

  [[nodiscard]] ResolveStatus resolve_contracts(IbkrClient &client) noexcept {
    for (std::size_t index = 0; index < slots.size(); ++index) {
      if (terminal_failure.load(std::memory_order_acquire)) {
        return ResolveStatus::Fatal;
      }
      if (phase_aborted()) {
        return ResolveStatus::RetrySession;
      }
      auto &slot = slots[index];
      if (slot.isolated.load(std::memory_order_acquire) ||
          (universe_selected && !slot.selected)) {
        continue;
      }
      const bool recheck = slot.accepted;
      reset_collection(index);
      if (!recheck) {
        slot.contract_state.store(0, std::memory_order_release);
      }
      int req_id = 0;
      const char *fatal = nullptr;
      if (!begin_contract_request(index, req_id, fatal)) {
        fail_terminal(fatal);
        return ResolveStatus::Fatal;
      }
      std::string request_error;
      if (!client.request_contract_details(req_id, contract_request(slot, recheck),
                                           request_error)) {
        std::cerr << "ibkr contract-details send-failed symbol="
                  << slot.spec.canonical_symbol << " req_id=" << req_id
                  << " session_generation="
                  << session_generation.load(std::memory_order_relaxed)
                  << " action=retry_session\n";
        request_session_retry("contract request failed");
        return ResolveStatus::RetrySession;
      }
      const auto contract_deadline =
          Clock::now() + std::chrono::milliseconds(options.connect_timeout_ms);
      while (!collection_finished(index) && Clock::now() < contract_deadline &&
             !phase_aborted()) {
        client.pump(std::chrono::milliseconds(10));
      }
      if (terminal_failure.load(std::memory_order_acquire)) {
        return ResolveStatus::Fatal;
      }
      if (phase_aborted()) {
        return ResolveStatus::RetrySession;
      }
      if (!collection_finished(index)) {
        std::cerr << "ibkr contract-details timeout symbol="
                  << slot.spec.canonical_symbol << " req_id=" << req_id
                  << " session_generation="
                  << session_generation.load(std::memory_order_relaxed)
                  << " action=retry_session\n";
        request_session_retry("contract details timed out");
        return ResolveStatus::RetrySession;
      }
      decide_slot(index, recheck);
    }
    if (phase_aborted()) {
      return terminal_failure.load(std::memory_order_acquire)
                 ? ResolveStatus::Fatal
                 : ResolveStatus::RetrySession;
    }
    if (!universe_selected) {
      settle_identities();
    }
    return ResolveStatus::Complete;
  }

  [[nodiscard]] bool subscribe_market_data(IbkrClient &client) noexcept {
    const auto pacing = std::max(options.subscribe_pacing_per_second, 1);
    const auto gap = std::chrono::milliseconds(1000 / pacing);
    bool first_subscribe = true;
    for (std::size_t index = 0; index < slots.size(); ++index) {
      if (phase_aborted()) {
        return false;
      }
      auto &slot = slots[index];
      if (!slot.selected || slot.isolated.load(std::memory_order_acquire) ||
          slot.suppressed.load(std::memory_order_acquire) ||
          !slot.ready.load(std::memory_order_acquire) ||
          slot.contract_state.load(std::memory_order_acquire) != 1) {
        continue;
      }
      if (!first_subscribe && gap.count() > 0 && !wait_interval(gap)) {
        return false;
      }
      first_subscribe = false;
      std::string request_error;
      if (!client.request_market_data(static_cast<int>(index + 1),
                                      contract_request(slot, true), "",
                                      request_error)) {
        deactivate_market_slot(slot);
        std::cerr << "ibkr market-data send-failed symbol="
                  << slot.spec.canonical_symbol << " action=retry_session\n";
        request_session_retry("market data request failed");
        return false;
      }
      if (phase_aborted()) {
        return false;
      }
      activate_market_slot(slot);
    }
    note_health(nullptr);
    return !phase_aborted();
  }

  static void reader_hook(void *context, publish::WirePublisher &) noexcept {
    static_cast<Impl *>(context)->publish_all_catalogs(true);
  }
};

IbkrRuntime::IbkrRuntime(RuntimeOptions options, IbkrClient &client)
    : options_(options),
      client_(client),
      impl_(std::make_unique<Impl>(std::move(options))) {}

IbkrRuntime::~IbkrRuntime() { stop(); }

utils::md::InstrumentId IbkrRuntime::instrument_id(std::size_t index) const {
  return impl_->slots.at(index).instrument_id;
}

std::uint32_t IbkrRuntime::generation() const noexcept {
  return impl_->generation.load(std::memory_order_acquire);
}

SessionState IbkrRuntime::state() const noexcept {
  return impl_->state.load(std::memory_order_acquire);
}

bool IbkrRuntime::live() const noexcept {
  return state() == SessionState::Live;
}

bool IbkrRuntime::failed() const noexcept {
  return impl_->terminal_failure.load(std::memory_order_acquire);
}

int IbkrRuntime::exit_code() const noexcept {
  return failed() ? impl_->process_exit_code.load(std::memory_order_acquire) : 0;
}

std::string IbkrRuntime::failure_message() const {
  std::lock_guard lock(impl_->failure_mu);
  return impl_->terminal_error;
}

RuntimeMetrics IbkrRuntime::metrics() const noexcept {
  RuntimeMetrics result;
  result.publish_count = impl_->publish_count.load(std::memory_order_relaxed);
  result.mailbox_overwrite_count =
      impl_->overwrite_count.load(std::memory_order_relaxed);
  result.wake_syscall_count = impl_->wake.syscalls();
  result.catalog_failures =
      impl_->catalog_failures.load(std::memory_order_relaxed);
  result.informational_errors =
      impl_->informational_errors.load(std::memory_order_relaxed);
  result.metric_success_average_volume =
      impl_->metric_success_average_volume.load(std::memory_order_relaxed);
  result.metric_success_market_cap =
      impl_->metric_success_market_cap.load(std::memory_order_relaxed);
  result.metric_success_reference_price =
      impl_->metric_success_reference_price.load(std::memory_order_relaxed);
  result.metric_missing_reference_price =
      impl_->metric_missing_reference_price.load(std::memory_order_relaxed);
  result.metric_persisted_turnover =
      impl_->metric_persisted_turnover.load(std::memory_order_relaxed);
  result.filtered_below_threshold =
      impl_->filtered_below_threshold.load(std::memory_order_relaxed);
  result.missing_metric_rejected =
      impl_->missing_metric_rejected.load(std::memory_order_relaxed);
  result.non_stock_exempt =
      impl_->non_stock_exempt.load(std::memory_order_relaxed);
  result.truncated = impl_->truncated.load(std::memory_order_relaxed);
  result.selected_non_stock =
      impl_->selected_non_stock.load(std::memory_order_relaxed);
  result.selected_stock =
      impl_->selected_stock.load(std::memory_order_relaxed);
  result.selected_subscriptions =
      impl_->selected_subscriptions.load(std::memory_order_relaxed);
  result.active_subscriptions =
      impl_->active_subscriptions.load(std::memory_order_relaxed);
  result.catalog_publish_count =
      impl_->catalog_publish_count.load(std::memory_order_relaxed);
  result.bbo_publish_count = result.publish_count;
  result.session_generation =
      impl_->session_generation.load(std::memory_order_relaxed);
  result.stale_contract_callbacks =
      impl_->stale_contract_callbacks.load(std::memory_order_relaxed);
  result.market_cap_filter_degraded = impl_->market_cap_filter_degraded;
  for (const auto &slot : impl_->slots) {
    result.snapshot_retry_count += slot.mailbox.retries();
    if (slot.assembler.has_value()) {
      result.invalid_tick_count += slot.assembler->invalid_ticks();
      result.non_live_count += slot.assembler->non_live_ticks();
    }
  }
  return result;
}

bool IbkrRuntime::start(std::string &error) {
  if (options_.subscriptions.empty()) {
    error = "ibkr runtime requires at least one subscription";
    return false;
  }
  if (options_.filter.max_subscriptions == 0) {
    error = "ibkr runtime requires max_subscriptions";
    return false;
  }

  for (const auto &spec : options_.subscriptions) {
    impl_->add_subscription(spec);
  }

  segment_name_ = publish::make_multiplex_segment_name(
      options_.shm_prefix, "ibkr", "equity", "ticker", 0);
  if (segment_name_.empty()) {
    error = "failed to build IBKR multiplex segment name";
    return false;
  }
  transport::RingOptions ring;
  ring.name = segment_name_;
  ring.ring_bytes = options_.ring_bytes;
  ring.max_record_bytes = options_.max_record_bytes;
  ring.max_readers = options_.max_readers;
  ring.unlink_on_close = options_.unlink_on_shutdown;
  ring.create = true;
  auto opened = transport::SharedRing::open(ring);
  if (!opened) {
    error = opened.message.empty() ? "failed to open IBKR shared ring"
                                   : opened.message;
    return false;
  }
  impl_->publisher =
      std::make_unique<publish::WirePublisher>(std::move(opened.value));
  impl_->publisher->set_reader_change_hook(impl_.get(), Impl::reader_hook);

  impl_->event_fd = ::eventfd(0, EFD_CLOEXEC | EFD_NONBLOCK);
  if (impl_->event_fd < 0) {
    error = "failed to create IBKR wake eventfd";
    impl_->publisher.reset();
    return false;
  }
  impl_->wake.set_fd(impl_->event_fd);
  impl_->backoff_ms = std::max(options_.reconnect_initial_ms, 1);
  client_.set_listener(this);
  impl_->stopping.store(false, std::memory_order_release);

  impl_->publisher_thread = std::thread([this] {
    impl_->publish_all_catalogs(false);
    impl_->publisher_ready.store(true, std::memory_order_release);
    while (!impl_->stopping.load(std::memory_order_acquire)) {
      pollfd descriptor{};
      descriptor.fd = impl_->event_fd;
      descriptor.events = POLLIN;
      (void)::poll(&descriptor, 1, 50);
      impl_->wake.clear();
      std::uint64_t drained = 0;
      while (::read(impl_->event_fd, &drained, sizeof(drained)) > 0) {
      }
      impl_->publish_all_catalogs(false);
      const auto generation =
          impl_->generation.load(std::memory_order_acquire);
      for (auto &slot : impl_->slots) {
        if (!slot.ready.load(std::memory_order_acquire) ||
            !slot.dirty.exchange(false, std::memory_order_acq_rel)) {
          continue;
        }
        BboSnapshot snapshot{};
        if (!slot.mailbox.read(snapshot)) {
          slot.dirty.store(true, std::memory_order_release);
          continue;
        }
        if (!snapshot.publishable || snapshot.book_generation != generation ||
            !slot.catalog_ok.load(std::memory_order_acquire)) {
          continue;
        }
        utils::md::BboEvent event{};
        event.header.instrument_id = slot.instrument_id;
        event.header.book_generation = snapshot.book_generation;
        event.header.source_seq = ++slot.source_seq;
        event.header.exchange_ts_ns = 0;
        event.header.receive_tsc = snapshot.receive_tsc;
        event.header.publish_tsc = sample_tsc();
        event.header.state = utils::md::BookState::Live;
        event.header.bbo_origin = utils::md::BboOrigin::TickerStream;
        event.bid = {.price = snapshot.bid_price, .quantity = snapshot.bid_size};
        event.ask = {.price = snapshot.ask_price, .quantity = snapshot.ask_size};
        if (!impl_->publisher->publish_bbo(event)) {
          slot.dirty.store(true, std::memory_order_release);
          continue;
        }
        const auto published =
            impl_->publish_count.fetch_add(1, std::memory_order_relaxed) + 1;
        if (published == 1) {
          impl_->note_health("first_bbo");
        }
      }
      if (impl_->publisher != nullptr) {
        (void)impl_->publisher->poll_reader_change();
      }
    }
  });

  impl_->control_thread = std::thread([this] {
    std::mt19937 random{std::random_device{}()};
    while (!impl_->stopping.load(std::memory_order_acquire) &&
           !impl_->terminal_failure.load(std::memory_order_acquire)) {
      impl_->session_retry_requested.store(false, std::memory_order_release);
      impl_->session_generation.fetch_add(1, std::memory_order_acq_rel);
      impl_->set_state(SessionState::Connecting);
      impl_->connected.store(false, std::memory_order_release);
      std::string connect_error;
      IbkrConnectParams params;
      params.host = options_.host;
      params.port = options_.port;
      params.client_id = options_.client_id;
      params.timeout_ms = options_.connect_timeout_ms;
      const bool connected = client_.connect(params, connect_error);
      if (!connected || impl_->phase_aborted()) {
        impl_->set_state(SessionState::Reconnecting);
      } else {
        impl_->set_state(SessionState::Handshaking);
        const auto deadline =
            Clock::now() + std::chrono::milliseconds(options_.connect_timeout_ms);
        while (!impl_->phase_aborted() && Clock::now() < deadline &&
               !impl_->connected.load(std::memory_order_acquire)) {
          client_.pump(std::chrono::milliseconds(20));
        }
      }
      auto status = Impl::ResolveStatus::RetrySession;
      auto ready_to_subscribe = false;
      if (impl_->connected.load(std::memory_order_acquire) &&
          !impl_->phase_aborted()) {
        client_.select_live_market_data();
        impl_->set_state(SessionState::ResolvingContracts);
        status = impl_->resolve_contracts(client_);
        if (status == Impl::ResolveStatus::Fatal ||
            impl_->terminal_failure.load(std::memory_order_acquire)) {
          break;
        }
        if (status == Impl::ResolveStatus::Complete && !impl_->phase_aborted()) {
          ready_to_subscribe = true;
          if (!impl_->universe_selected) {
            impl_->set_state(SessionState::ProbingMetrics);
            ready_to_subscribe = impl_->probe_metrics(client_);
            if (ready_to_subscribe && !impl_->phase_aborted()) {
              impl_->persist_turnover_metrics();
              impl_->select_universe();
            } else {
              ready_to_subscribe = false;
            }
          }
          if (ready_to_subscribe && !impl_->phase_aborted()) {
            impl_->set_state(SessionState::Subscribing);
            ready_to_subscribe = impl_->subscribe_market_data(client_);
          }
          if (ready_to_subscribe && !impl_->phase_aborted()) {
            impl_->set_state(SessionState::Live);
            impl_->backoff_ms = std::max(options_.reconnect_initial_ms, 1);
            auto last_summary = Clock::now();
            while (!impl_->phase_aborted()) {
              client_.pump(std::chrono::milliseconds(
                  std::max(options_.heartbeat_interval_ms, 20)));
              if (Clock::now() - last_summary >= kHealthSummaryInterval) {
                impl_->note_health("summary");
                last_summary = Clock::now();
              }
            }
          }
        }
      }
      if (impl_->stopping.load(std::memory_order_acquire) ||
          impl_->terminal_failure.load(std::memory_order_acquire)) {
        break;
      }
      impl_->set_state(SessionState::Reconnecting);
      impl_->advance_generation_and_clear_books();
      impl_->clear_market_active();
      impl_->clear_pending_contracts();
      impl_->session_retry_requested.store(false, std::memory_order_release);
      client_.disconnect();
      if (impl_->backoff_ms <= 0) {
        impl_->backoff_ms = std::max(options_.reconnect_initial_ms, 1);
      }
      std::uniform_int_distribution<int> jitter(
          0, std::max(impl_->backoff_ms / 5, 1));
      std::this_thread::sleep_for(
          std::chrono::milliseconds(impl_->backoff_ms + jitter(random)));
      impl_->backoff_ms = std::min(impl_->backoff_ms * 2,
                                   std::max(options_.reconnect_max_ms, 1));
    }
    client_.disconnect();
    impl_->clear_pending_contracts();
    impl_->set_state(SessionState::Disconnected);
  });

  const auto ready_deadline = Clock::now() + std::chrono::seconds(2);
  while (Clock::now() < ready_deadline &&
         !impl_->publisher_ready.load(std::memory_order_acquire)) {
    std::this_thread::sleep_for(std::chrono::milliseconds(5));
  }
  if (!impl_->publisher_ready.load(std::memory_order_acquire)) {
    error = "IBKR publisher thread did not become ready";
    stop();
    return false;
  }
  return true;
}

void IbkrRuntime::stop() noexcept {
  if (impl_ == nullptr) {
    return;
  }
  impl_->stopping.store(true, std::memory_order_release);
  impl_->wake.force();
  client_.disconnect();
  if (impl_->publisher_thread.joinable()) {
    impl_->publisher_thread.join();
  }
  if (impl_->control_thread.joinable()) {
    impl_->control_thread.join();
  }
  impl_->publisher.reset();
  if (impl_->event_fd >= 0) {
    ::close(impl_->event_fd);
    impl_->event_fd = -1;
  }
}

void IbkrRuntime::on_connected() noexcept {
  impl_->connected.store(true, std::memory_order_release);
}

void IbkrRuntime::on_disconnected() noexcept {
  impl_->connected.store(false, std::memory_order_release);
  if (impl_->stopping.load(std::memory_order_acquire) ||
      impl_->terminal_failure.load(std::memory_order_acquire)) {
    return;
  }
  const auto current = impl_->state.load(std::memory_order_acquire);
  if (current == SessionState::Live || current == SessionState::Subscribing ||
      current == SessionState::ProbingMetrics ||
      current == SessionState::ResolvingContracts ||
      current == SessionState::Handshaking) {
    if (!impl_->session_retry_requested.load(std::memory_order_acquire)) {
      impl_->invalidate_books();
    }
    impl_->request_session_retry("disconnected");
  }
}

void IbkrRuntime::on_contract_details(
    int req_id, const IbkrContractDetails &details) noexcept {
  std::size_t index = 0;
  if (impl_->match_pending_contract(req_id, index, false, true) !=
      Impl::PendingMatch::Current) {
    return;
  }
  auto &collection = *impl_->collections[index];
  std::lock_guard lock(collection.mutex);
  collection.rows.push_back(details);
}

void IbkrRuntime::on_contract_details_end(int req_id) noexcept {
  std::size_t index = 0;
  if (impl_->match_pending_contract(req_id, index, true, true) !=
      Impl::PendingMatch::Current) {
    return;
  }
  auto &collection = *impl_->collections[index];
  std::lock_guard lock(collection.mutex);
  collection.ended = true;
}

void IbkrRuntime::on_tick_price(int ticker_id, bool bid, double price,
                                std::uint64_t receive_tsc) noexcept {
  auto *slot = impl_->slot_for(ticker_id);
  if (slot == nullptr || !impl_->quotes_accepted() ||
      !slot->ready.load(std::memory_order_acquire) ||
      !slot->assembler.has_value() ||
      slot->isolated.load(std::memory_order_acquire) ||
      slot->suppressed.load(std::memory_order_acquire)) {
    return;
  }
  const auto generation = impl_->generation.load(std::memory_order_acquire);
  if (slot->assembler->generation() != generation) {
    slot->assembler->reset(generation);
  }
  const auto update = slot->assembler->on_price(bid, price, generation);
  impl_->note_quote(*slot, update, receive_tsc);
}

void IbkrRuntime::on_reference_price(int ticker_id,
                                     ReferencePriceSource source,
                                     double price) noexcept {
  std::size_t index = 0;
  if (impl_->state.load(std::memory_order_acquire) !=
          SessionState::ProbingMetrics ||
      !metric_index(ticker_id, impl_->slots.size(), index) ||
      source == ReferencePriceSource::None || !std::isfinite(price) ||
      price <= 0.0) {
    return;
  }
  auto &slot = impl_->slots[index];
  std::lock_guard lock(slot.metric_mutex);
  if (reference_price_priority(source) <
      reference_price_priority(slot.reference_price_source)) {
    return;
  }
  slot.reference_price_usd = price;
  slot.reference_price_source = source;
}

void IbkrRuntime::on_tick_size(int ticker_id, bool bid,
                               std::string_view size_text,
                               std::uint64_t receive_tsc) noexcept {
  auto *slot = impl_->slot_for(ticker_id);
  if (slot == nullptr || !impl_->quotes_accepted() ||
      !slot->ready.load(std::memory_order_acquire) ||
      !slot->assembler.has_value() ||
      slot->isolated.load(std::memory_order_acquire) ||
      slot->suppressed.load(std::memory_order_acquire)) {
    return;
  }
  const auto generation = impl_->generation.load(std::memory_order_acquire);
  if (slot->assembler->generation() != generation) {
    slot->assembler->reset(generation);
  }
  const auto update = slot->assembler->on_size(bid, size_text, generation);
  impl_->note_quote(*slot, update, receive_tsc);
}

void IbkrRuntime::on_average_volume(
    int ticker_id, std::string_view size_text) noexcept {
  std::size_t index = 0;
  if (!metric_index(ticker_id, impl_->slots.size(), index)) {
    return;
  }
  const auto parsed = parse_nonnegative(size_text);
  auto &slot = impl_->slots[index];
  std::lock_guard lock(slot.metric_mutex);
  slot.average_volume_received = true;
  if (parsed.has_value()) {
    // IB stock volume ticks are reported in round lots.
    slot.metrics.average_volume_90d = *parsed * 100.0;
  }
}

void IbkrRuntime::on_fundamental_ratios(
    int ticker_id, std::string_view ratios) noexcept {
  std::size_t index = 0;
  if (!metric_index(ticker_id, impl_->slots.size(), index)) {
    return;
  }
  const auto market_cap = parse_market_cap(ratios);
  auto &slot = impl_->slots[index];
  std::lock_guard lock(slot.metric_mutex);
  slot.market_cap_received = true;
  slot.metrics.market_cap_usd = market_cap;
}

void IbkrRuntime::on_market_data_type(int ticker_id,
                                      MarketDataKind kind) noexcept {
  auto *slot = impl_->slot_for(ticker_id);
  if (slot == nullptr || !impl_->quotes_accepted() ||
      !slot->ready.load(std::memory_order_acquire) ||
      !slot->assembler.has_value() ||
      slot->isolated.load(std::memory_order_acquire)) {
    return;
  }
  const auto generation = impl_->generation.load(std::memory_order_acquire);
  if (slot->assembler->generation() != generation) {
    slot->assembler->reset(generation);
  }
  const auto update = slot->assembler->on_market_data_type(kind, generation);
  impl_->note_quote(*slot, update, sample_tsc());
}

void IbkrRuntime::on_error(int id, int code, std::string_view message) noexcept {
  const auto klass = classify_ibkr_error(code);
  std::size_t contract_index = 0;
  const auto pending = impl_->match_pending_contract(id, contract_index, true,
                                                     false);
  const char *request_kind = "session";
  const char *symbol = "";
  if (pending == Impl::PendingMatch::Current) {
    request_kind = "contract_details";
    symbol = impl_->slots[contract_index].spec.canonical_symbol.c_str();
    if (klass == IbkrErrorClass::Contract) {
      impl_->isolate_slot(impl_->slots[contract_index], "contract error");
    }
  } else if (pending == Impl::PendingMatch::Stale || id >= kContractReqIdBase) {
    request_kind = pending == Impl::PendingMatch::Stale ? "stale_contract_details"
                                                        : "contract_details";
  } else if (auto *slot = impl_->slot_for(id)) {
    request_kind = "market_data";
    symbol = slot->spec.canonical_symbol.c_str();
    if (market_data_denied(code)) {
      impl_->deactivate_market_slot(*slot);
      slot->suppressed.store(true, std::memory_order_release);
      BboSnapshot empty{};
      empty.book_generation = impl_->generation.load(std::memory_order_acquire);
      empty.publishable = false;
      slot->mailbox.write(empty);
      mark_dirty(slot->dirty, impl_->overwrite_count);
      impl_->note_health(nullptr);
    }
  } else if (klass == IbkrErrorClass::Informational) {
    impl_->note_informational(code, message);
    return;
  } else {
    switch (klass) {
      case IbkrErrorClass::ConnectionLost:
        if (code == 1100 &&
            !impl_->session_retry_requested.load(std::memory_order_acquire)) {
          impl_->invalidate_books();
        }
        impl_->request_session_retry(code == 1100 ? "1100" : "connection lost");
        break;
      case IbkrErrorClass::ConnectionRestoredDataLost:
        impl_->request_session_retry("1101");
        break;
      case IbkrErrorClass::ConnectionRestoredDataMaintained:
        break;
      default:
        break;
    }
  }
  if (klass == IbkrErrorClass::Informational) {
    return;
  }
  if (klass == IbkrErrorClass::ConnectionRestoredDataMaintained ||
      klass == IbkrErrorClass::ConnectionLost ||
      klass == IbkrErrorClass::ConnectionRestoredDataLost ||
      klass == IbkrErrorClass::Contract ||
      klass == IbkrErrorClass::MarketDataDenied) {
    std::cerr << "ibkr error id=" << id << " code=" << code << " message=\""
              << message << "\" class=" << error_class_name(klass)
              << " session_state="
              << session_state_name(impl_->state.load(std::memory_order_acquire))
              << " session_generation="
              << impl_->session_generation.load(std::memory_order_relaxed)
              << " request_kind=" << request_kind;
    if (symbol != nullptr && symbol[0] != '\0') {
      std::cerr << " symbol=" << symbol;
    }
    std::cerr << '\n';
  }
}

}  // namespace mds::ibkr

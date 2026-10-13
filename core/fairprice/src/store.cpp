#include "fairprice/store.h"

#include "fairprice/sqab.h"

#include <algorithm>
#include <chrono>
#include <cmath>
#include <cstring>

namespace fairprice {
namespace {

constexpr std::size_t kSampleCapacity = 20 * 60 * 60;

std::uint64_t hour_of(std::uint64_t wall_ns) {
  return wall_ns / 3'600'000'000'000ULL;
}

bool identity_from_bbo(const BboView &bbo) {
  return bbo.wall_ns != 0 || bbo.record.member_count != 0 ||
         bbo.record.base_asset[0] != '\0';
}

bool identity_from_book(const BookSnapshot &book) {
  return book.wall_ns != 0 || book.member_count != 0 || book.base_asset[0] != '\0';
}

}  // namespace

BookSnapshot book_snapshot_from_record(
    const utils::md::wire::AggOrderBookRecord &record) {
  BookSnapshot book{};
  book.exchange_ts_ns = record.header.exchange_ts_ns;
  book.price_scale = record.price_scale;
  book.quantity_scale = record.quantity_scale;
  book.member_count = record.member_count;
  book.member_mask = record.member_mask;
  book.active_mask = record.active_mask;
  book.venue_slot_ids = record.venue_slot_ids;
  book.base_asset = record.base_asset;
  book.quote_asset = record.quote_asset;
  book.bid_count = std::min(kMaxDepth, static_cast<int>(record.bid_count));
  book.ask_count = std::min(kMaxDepth, static_cast<int>(record.ask_count));
  for (int i = 0; i < book.bid_count; ++i) {
    const auto &src = record.bids[static_cast<std::size_t>(i)];
    auto &dst = book.bids[static_cast<std::size_t>(i)];
    dst.price = src.price;
    dst.quantity = src.quantity;
    dst.venue_quantity = src.venue_quantity;
    dst.venue_mask = src.venue_mask;
    dst.contributor_count = src.contributor_count;
  }
  for (int i = 0; i < book.ask_count; ++i) {
    const auto &src = record.asks[static_cast<std::size_t>(i)];
    auto &dst = book.asks[static_cast<std::size_t>(i)];
    dst.price = src.price;
    dst.quantity = src.quantity;
    dst.venue_quantity = src.venue_quantity;
    dst.venue_mask = src.venue_mask;
    dst.contributor_count = src.contributor_count;
  }
  return book;
}

double signed_spread_bps(std::int64_t bid, std::int64_t ask) {
  if (bid <= 0 || ask <= 0) {
    return 0;
  }
  return static_cast<double>(ask - bid) * 20000.0 /
         (static_cast<double>(ask) + static_cast<double>(bid));
}

Store::Store(std::optional<Engine> engine, std::uint64_t stale_after_ns)
    : engine_(std::move(engine)), stale_after_ns_(stale_after_ns) {}

std::string Store::model_id() const {
  return engine_ ? engine_->model_id() : std::string{};
}

bool Store::fresh(std::uint64_t wall_ns, std::uint64_t now_ns) const {
  if (stale_after_ns_ == 0) {
    return true;
  }
  return wall_ns <= now_ns && now_ns - wall_ns <= stale_after_ns_;
}

void Store::reconcile(const CatalogSnapshot &catalog) {
  std::lock_guard<std::mutex> lock(mutex_);
  std::unordered_map<std::string, std::unique_ptr<Market>> next;
  std::unordered_map<std::string, std::vector<std::string>> index;
  for (const auto &entry : catalog.markets) {
    const auto key = market_key(entry.profile, entry.symbol);
    auto market = std::make_unique<Market>();
    market->catalog = entry;
    market->samples.resize(kSampleCapacity);
    if (auto it = markets_.find(key); it != markets_.end()) {
      if (entry.bbo_segment == it->second->catalog.bbo_segment) {
        market->bbo = it->second->bbo;
        market->bbo_seq = it->second->bbo_seq;
        market->samples = it->second->samples;
        market->sample_next = it->second->sample_next;
        market->sample_full = it->second->sample_full;
        market->sample_hour = it->second->sample_hour;
      }
      if (entry.book_segment == it->second->catalog.book_segment) {
        market->book = it->second->book;
        market->fair = it->second->fair;
        market->ewma = it->second->ewma;
        market->has_ewma = it->second->has_ewma;
        market->book_seq = it->second->book_seq;
        market->fair_json = it->second->fair_json;
        market->fair_json_generation = it->second->fair_json_generation;
      }
    }
    index[canonical_symbol(entry.symbol)].push_back(key);
    next.emplace(key, std::move(market));
  }
  markets_ = std::move(next);
  symbol_index_ = std::move(index);
}

std::string Store::check_sequence(SequenceState &state, std::uint64_t epoch,
                                  std::uint64_t sequence,
                                  std::uint64_t generation) {
  if (state.has && epoch == state.epoch) {
    if (sequence < state.sequence) {
      return "regressive sequence";
    }
    if (sequence == state.sequence) {
      return "duplicate";
    }
    if (generation < state.generation) {
      return "generation rollback";
    }
  }
  state = SequenceState{true, epoch, sequence, generation};
  return {};
}

std::string Store::apply_bbo(Market &market, std::uint64_t epoch,
                             std::uint64_t sequence, std::uint64_t generation,
                             std::uint64_t wall_ns,
                             const utils::md::wire::AggBboRecord &record) {
  const auto seq_error =
      check_sequence(market.bbo_seq, epoch, sequence, generation);
  if (seq_error == "duplicate") {
    return {};
  }
  if (!seq_error.empty()) {
    return seq_error;
  }
  market.bbo.ready = true;
  market.bbo.ring_epoch = epoch;
  market.bbo.ring_sequence = sequence;
  market.bbo.generation = generation;
  market.bbo.wall_ns = wall_ns;
  market.bbo.record = record;
  const auto hour = hour_of(wall_ns);
  if (hour != market.sample_hour) {
    market.sample_hour = hour;
    market.sample_next = 0;
    market.sample_full = false;
  }
  LiveSample sample{};
  sample.wall_ns = wall_ns;
  sample.raw_bps = signed_spread_bps(record.raw_bid.price, record.raw_ask.price);
  sample.gated_bps =
      signed_spread_bps(record.gated_bid.price, record.gated_ask.price);
  sample.raw_min = sample.raw_bps;
  sample.raw_max = sample.raw_bps;
  sample.gated_min = sample.gated_bps;
  sample.gated_max = sample.gated_bps;
  sample.window_samples = 1;
  if (!market.samples.empty()) {
    market.samples[market.sample_next] = sample;
    market.sample_next = (market.sample_next + 1) % market.samples.size();
    if (market.sample_next == 0) {
      market.sample_full = true;
    }
  }
  market.bbo.browser = encode_sqab_bbo(market.catalog.symbol, market.bbo);
  if (market.bbo.browser) {
    sqab_encode_count_.fetch_add(1);
  }
  return {};
}

std::string Store::apply_book(Market &market, std::uint64_t epoch,
                              std::uint64_t sequence, std::uint64_t generation,
                              std::uint64_t wall_ns, const BookSnapshot &book,
                              ComputeStats *stats) {
  const auto seq_error =
      check_sequence(market.book_seq, epoch, sequence, generation);
  if (seq_error == "duplicate") {
    return {};
  }
  if (!seq_error.empty()) {
    return seq_error;
  }
  market.book = book;
  market.book.ready = true;
  market.book.ring_epoch = epoch;
  market.book.ring_sequence = sequence;
  market.book.generation = generation;
  market.book.wall_ns = wall_ns;
  market.book.browser20 = encode_sqab_book(market.catalog.symbol, market.book, 20);
  market.book.browser50 = encode_sqab_book(market.catalog.symbol, market.book, 50);
  if (market.book.browser20) {
    sqab_encode_count_.fetch_add(1);
  }
  if (market.book.browser50) {
    sqab_encode_count_.fetch_add(1);
  }
  if (!engine_ || !engine_->config().enabled) {
    if (stats) {
      *stats = {};
    }
    return {};
  }
  SnapshotView view{};
  view.ready = true;
  view.ring_epoch = epoch;
  view.ring_sequence = sequence;
  view.generation = generation;
  view.wall_ns = wall_ns;
  BookView book_view{};
  book_view.exchange_ts_ns = book.exchange_ts_ns;
  book_view.price_scale = book.price_scale;
  book_view.quantity_scale = book.quantity_scale;
  book_view.member_count = book.member_count;
  book_view.member_mask = book.member_mask;
  book_view.active_mask = book.active_mask;
  book_view.bids = {book.bids.data(), static_cast<std::size_t>(book.bid_count)};
  book_view.asks = {book.asks.data(), static_cast<std::size_t>(book.ask_count)};
  view.book = &book_view;
  ModelState next{};
  market.fair = engine_->compute(view, market.has_ewma ? &market.ewma : nullptr,
                                 &next, stats);
  if (market.fair.ready) {
    market.ewma = next;
    market.has_ewma = true;
  } else {
    market.has_ewma = false;
  }
  market.fair_json.reset();
  market.fair_json_generation = static_cast<std::uint64_t>(-1);
  return {};
}

std::string Store::apply_bbo_key(const std::string &key, std::uint64_t epoch,
                                 std::uint64_t sequence, std::uint64_t generation,
                                 std::uint64_t wall_ns,
                                 const utils::md::wire::AggBboRecord &record,
                                 FairPricePod *published) {
  std::lock_guard<std::mutex> lock(mutex_);
  auto it = markets_.find(key);
  if (it == markets_.end()) {
    return "unknown market";
  }
  if (published) {
    *published = {};
  }
  return apply_bbo(*it->second, epoch, sequence, generation, wall_ns, record);
}

std::string Store::apply_book_key(const std::string &key, std::uint64_t epoch,
                                  std::uint64_t sequence, std::uint64_t generation,
                                  std::uint64_t wall_ns, const BookSnapshot &book,
                                  FairPricePod *published, ComputeStats *stats) {
  std::lock_guard<std::mutex> lock(mutex_);
  auto it = markets_.find(key);
  if (it == markets_.end()) {
    return "unknown market";
  }
  auto error = apply_book(*it->second, epoch, sequence, generation, wall_ns, book,
                          stats);
  if (published) {
    *published = it->second->fair;
  }
  return error;
}

RestoreResult Store::restore_bbo_if_empty(
    const std::string &key, std::uint64_t epoch, std::uint64_t sequence,
    std::uint64_t generation, std::uint64_t wall_ns,
    const utils::md::wire::AggBboRecord &record, std::uint64_t now_ns) {
  std::lock_guard<std::mutex> lock(mutex_);
  auto it = markets_.find(key);
  if (it == markets_.end() || identity_from_bbo(it->second->bbo)) {
    return {};
  }
  auto error = apply_bbo(*it->second, epoch, sequence, generation, wall_ns, record);
  if (!error.empty()) {
    return {};
  }
  it->second->bbo_seq.has = false;
  RestoreResult result;
  result.restored = true;
  result.ready = it->second->bbo.ready;
  result.fresh = fresh(it->second->bbo.wall_ns, now_ns);
  result.epoch = epoch;
  result.sequence = sequence;
  result.generation = generation;
  return result;
}

RestoreResult Store::restore_book_if_empty(const std::string &key,
                                           std::uint64_t epoch,
                                           std::uint64_t sequence,
                                           std::uint64_t generation,
                                           std::uint64_t wall_ns,
                                           const BookSnapshot &book,
                                           std::uint64_t now_ns) {
  std::lock_guard<std::mutex> lock(mutex_);
  auto it = markets_.find(key);
  if (it == markets_.end() || identity_from_book(it->second->book)) {
    return {};
  }
  auto error = apply_book(*it->second, epoch, sequence, generation, wall_ns, book);
  if (!error.empty()) {
    return {};
  }
  it->second->book_seq.has = false;
  RestoreResult result;
  result.restored = true;
  result.ready = it->second->book.ready;
  result.fresh = fresh(it->second->book.wall_ns, now_ns);
  result.epoch = epoch;
  result.sequence = sequence;
  result.generation = generation;
  result.fair = it->second->fair;
  return result;
}

bool Store::book_has_live_sequence(const std::string &key) const {
  std::lock_guard<std::mutex> lock(mutex_);
  auto it = markets_.find(key);
  return it != markets_.end() && it->second->book_seq.has;
}

bool Store::should_discard_startup_publish(const std::string &key,
                                           std::uint64_t epoch,
                                           std::uint64_t sequence,
                                           std::uint64_t generation) const {
  std::lock_guard<std::mutex> lock(mutex_);
  auto it = markets_.find(key);
  if (it == markets_.end()) {
    return true;
  }
  const auto &market = *it->second;
  if (market.book_seq.has) {
    return true;
  }
  if (market.book.ring_epoch != epoch || market.book.ring_sequence != sequence) {
    return true;
  }
  (void)generation;
  return false;
}

bool Store::reset_bbo(const std::string &key, std::string_view reason,
                      std::uint64_t epoch, std::uint64_t sequence,
                      std::uint64_t generation, FairPricePod *published) {
  std::lock_guard<std::mutex> lock(mutex_);
  auto it = markets_.find(key);
  if (it == markets_.end()) {
    return false;
  }
  (void)published;
  it->second->bbo_seq = {};
  mark_bbo_not_ready(*it->second, reason, epoch, sequence, generation);
  return true;
}

bool Store::reset_book(const std::string &key, std::string_view reason,
                       std::uint64_t epoch, std::uint64_t sequence,
                       std::uint64_t generation, FairPricePod *published) {
  std::lock_guard<std::mutex> lock(mutex_);
  auto it = markets_.find(key);
  if (it == markets_.end()) {
    return false;
  }
  it->second->book_seq = {};
  mark_book_not_ready(*it->second, reason, epoch, sequence, generation);
  if (published) {
    *published = it->second->fair;
  }
  return true;
}

bool Store::mark_stale(const std::string &key, std::uint64_t now_ns,
                       FairPricePod *published) {
  std::lock_guard<std::mutex> lock(mutex_);
  auto it = markets_.find(key);
  if (it == markets_.end()) {
    return false;
  }
  auto &market = *it->second;
  if (market.bbo.ready && !fresh(market.bbo.wall_ns, now_ns)) {
    mark_bbo_not_ready(market, "source_stale", market.bbo.ring_epoch,
                       market.bbo.ring_sequence, market.bbo.generation);
  }
  const bool book_stale =
      market.book.ready && !fresh(market.book.wall_ns, now_ns);
  const bool fair_stale =
      market.fair.ready && !fresh(market.fair.wall_ns, now_ns);
  if (!book_stale && !fair_stale) {
    return false;
  }
  auto generation = market.fair.generation ? market.fair.generation
                                           : market.book.generation;
  if (generation == 0) {
    generation = 1;
  } else {
    generation += 1;
  }
  mark_book_not_ready(market, "source_stale", market.book.ring_epoch,
                      market.book.ring_sequence, generation);
  if (published) {
    *published = market.fair;
  }
  return true;
}

void Store::mark_fair_not_ready(Market &market, std::string_view reason,
                                std::uint64_t epoch, std::uint64_t sequence,
                                std::uint64_t generation) {
  market.fair.ready = 0;
  copy_cstr(market.fair.reset_reason, reason);
  if (epoch || sequence || generation) {
    market.fair.ring_epoch = epoch;
    market.fair.ring_sequence = sequence;
    market.fair.generation = generation;
  }
  market.has_ewma = false;
  market.fair_json.reset();
  market.fair_json_generation = static_cast<std::uint64_t>(-1);
}

void Store::mark_bbo_not_ready(Market &market, std::string_view reason,
                               std::uint64_t epoch, std::uint64_t sequence,
                               std::uint64_t generation) {
  (void)reason;
  market.bbo.ready = false;
  market.bbo.browser.reset();
  if (epoch || sequence || generation) {
    market.bbo.ring_epoch = epoch;
    market.bbo.ring_sequence = sequence;
    market.bbo.generation = generation;
  }
}

void Store::mark_book_not_ready(Market &market, std::string_view reason,
                                std::uint64_t epoch, std::uint64_t sequence,
                                std::uint64_t generation) {
  market.book.ready = false;
  market.book.browser20.reset();
  market.book.browser50.reset();
  if (epoch || sequence || generation) {
    market.book.ring_epoch = epoch;
    market.book.ring_sequence = sequence;
    market.book.generation = generation;
  }
  mark_fair_not_ready(market, reason, epoch, sequence, generation);
}

void Store::refresh_fair_json(Market &market) {
  if (!market.fair.ready) {
    market.fair_json.reset();
    return;
  }
  const auto generation =
      market.fair.ring_sequence ^ (market.fair.generation << 1);
  if (market.fair_json && market.fair_json_generation == generation) {
    return;
  }
  market.fair_json = std::make_shared<const std::string>(encode_fair_price_json(
      market.catalog.profile, market.catalog.symbol, market.fair));
  market.fair_json_generation = generation;
}

void Store::refresh_fair_json_key(const std::string &key) {
  std::lock_guard<std::mutex> lock(mutex_);
  auto it = markets_.find(key);
  if (it != markets_.end()) {
    refresh_fair_json(*it->second);
  }
}

MarketView Store::view_of(const Market &market) const {
  MarketView view;
  view.catalog = market.catalog;
  view.bbo = market.bbo;
  view.book = market.book;
  view.fair = market.fair;
  view.fair_json = market.fair_json;
  return view;
}

std::optional<MarketView> Store::copy_view(const std::string &key) const {
  std::lock_guard<std::mutex> lock(mutex_);
  auto it = markets_.find(key);
  if (it == markets_.end()) {
    return std::nullopt;
  }
  return view_of(*it->second);
}

std::optional<MarketView> Store::copy_profile_symbol(std::string_view profile,
                                                     std::string_view symbol) const {
  return copy_view(market_key(profile, canonical_symbol(symbol)));
}

std::optional<MarketView> Store::copy_unique_symbol(std::string_view symbol,
                                                    std::string &error) const {
  std::lock_guard<std::mutex> lock(mutex_);
  auto it = symbol_index_.find(canonical_symbol(symbol));
  if (it == symbol_index_.end() || it->second.empty()) {
    error = "unknown symbol \"" + std::string(symbol) + "\"";
    return std::nullopt;
  }
  if (it->second.size() > 1) {
    error = "symbol \"" + std::string(symbol) + "\" exists in multiple profiles";
    return std::nullopt;
  }
  auto market = markets_.find(it->second.front());
  if (market == markets_.end()) {
    error = "unknown symbol \"" + std::string(symbol) + "\"";
    return std::nullopt;
  }
  return view_of(*market->second);
}

Market *Store::find(std::string_view profile, std::string_view symbol) {
  std::lock_guard<std::mutex> lock(mutex_);
  const auto key = market_key(profile, canonical_symbol(symbol));
  auto it = markets_.find(key);
  return it == markets_.end() ? nullptr : it->second.get();
}

Market *Store::find_by_key(const std::string &key) {
  std::lock_guard<std::mutex> lock(mutex_);
  auto it = markets_.find(key);
  return it == markets_.end() ? nullptr : it->second.get();
}

Market *Store::find_unique_symbol(std::string_view symbol, std::string &error) {
  std::lock_guard<std::mutex> lock(mutex_);
  auto it = symbol_index_.find(canonical_symbol(symbol));
  if (it == symbol_index_.end() || it->second.empty()) {
    error = "unknown symbol \"" + std::string(symbol) + "\"";
    return nullptr;
  }
  if (it->second.size() > 1) {
    error = "symbol \"" + std::string(symbol) + "\" exists in multiple profiles";
    return nullptr;
  }
  auto market = markets_.find(it->second.front());
  return market == markets_.end() ? nullptr : market->second.get();
}

std::vector<MarketJson> Store::markets_json(std::uint64_t now_ns) const {
  std::lock_guard<std::mutex> lock(mutex_);
  std::vector<MarketJson> result;
  result.reserve(markets_.size());
  for (const auto &[_, market] : markets_) {
    result.push_back(market_json_locked(*market, now_ns));
  }
  std::sort(result.begin(), result.end(),
            [](const MarketJson &left, const MarketJson &right) {
              if (left.symbol == right.symbol) {
                return left.profile < right.profile;
              }
              return left.symbol < right.symbol;
            });
  return result;
}

namespace {

std::string_view fixed_text(const char *data, std::size_t n) {
  std::size_t i = 0;
  while (i < n && data[i] != '\0') {
    ++i;
  }
  return {data, i};
}

void fill_identity(MarketJson &value, std::string_view base,
                   std::string_view quote, const std::array<std::uint8_t, 8> &slots,
                   std::uint8_t count, std::uint8_t price_scale,
                   std::uint8_t quantity_scale, bool ready, bool fresh_now,
                   std::uint64_t wall_ns) {
  value.base = std::string(base);
  value.quote = std::string(quote);
  value.price_scale = price_scale;
  value.quantity_scale = quantity_scale;
  value.venues.clear();
  for (std::uint8_t i = 0; i < count && i < 8; ++i) {
    value.venues.emplace_back(venue_name(slots[i]));
  }
  value.live = ready && fresh_now;
  value.updated_ns = wall_ns;
}

}  // namespace

MarketJson Store::market_json_locked(const Market &market,
                                     std::uint64_t now_ns) const {
  MarketJson value;
  value.profile = market.catalog.profile;
  value.symbol = market.catalog.symbol;
  value.has_bbo = !market.catalog.bbo_segment.empty();
  value.has_order_book = !market.catalog.book_segment.empty();
  value.recording = market.catalog.recording;
  if (identity_from_bbo(market.bbo)) {
    fill_identity(value, fixed_text(market.bbo.record.base_asset.data(),
                                    market.bbo.record.base_asset.size()),
                  fixed_text(market.bbo.record.quote_asset.data(),
                             market.bbo.record.quote_asset.size()),
                  market.bbo.record.venue_slot_ids, market.bbo.record.member_count,
                  market.bbo.record.price_scale, market.bbo.record.quantity_scale,
                  market.bbo.ready, fresh(market.bbo.wall_ns, now_ns),
                  market.bbo.wall_ns);
  } else if (identity_from_book(market.book)) {
    fill_identity(value, fixed_text(market.book.base_asset.data(),
                                    market.book.base_asset.size()),
                  fixed_text(market.book.quote_asset.data(),
                             market.book.quote_asset.size()),
                  market.book.venue_slot_ids, market.book.member_count,
                  market.book.price_scale, market.book.quantity_scale,
                  market.book.ready, fresh(market.book.wall_ns, now_ns),
                  market.book.wall_ns);
  }
  return value;
}

std::vector<Market *> Store::all_markets() {
  std::lock_guard<std::mutex> lock(mutex_);
  std::vector<Market *> result;
  result.reserve(markets_.size());
  for (auto &[_, market] : markets_) {
    result.push_back(market.get());
  }
  return result;
}

std::vector<MarketView> Store::copy_all() const {
  std::lock_guard<std::mutex> lock(mutex_);
  std::vector<MarketView> result;
  result.reserve(markets_.size());
  for (const auto &[_, market] : markets_) {
    result.push_back(view_of(*market));
  }
  return result;
}

std::vector<LiveSample> Store::copy_samples(const std::string &key) const {
  std::lock_guard<std::mutex> lock(mutex_);
  auto it = markets_.find(key);
  if (it == markets_.end()) {
    return {};
  }
  const auto &market = *it->second;
  const auto count =
      market.sample_full ? market.samples.size() : market.sample_next;
  const auto start = market.sample_full ? market.sample_next : 0;
  std::vector<LiveSample> live;
  live.reserve(count);
  for (std::size_t i = 0; i < count; ++i) {
    live.push_back(market.samples[(start + i) % market.samples.size()]);
  }
  return live;
}

bool Store::ready() const {
  using namespace std::chrono;
  const auto now = static_cast<std::uint64_t>(
      duration_cast<nanoseconds>(system_clock::now().time_since_epoch())
          .count());
  return ready(now);
}

bool Store::ready(std::uint64_t now_ns) const {
  std::lock_guard<std::mutex> lock(mutex_);
  if (markets_.empty()) {
    return false;
  }
  for (const auto &[_, market] : markets_) {
    if (!market->catalog.bbo_segment.empty()) {
      if (!market->bbo.ready || !fresh(market->bbo.wall_ns, now_ns)) {
        return false;
      }
    }
    if (!market->catalog.book_segment.empty()) {
      if (!market->book.ready || !fresh(market->book.wall_ns, now_ns)) {
        return false;
      }
    }
  }
  return true;
}

}  // namespace fairprice

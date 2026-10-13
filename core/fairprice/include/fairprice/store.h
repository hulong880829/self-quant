#pragma once

#include "fairprice/catalog.h"
#include "fairprice/engine.h"
#include "fairprice/json.h"

#include <atomic>
#include <memory>
#include <mutex>
#include <optional>
#include <string>
#include <unordered_map>
#include <vector>

namespace fairprice {

struct SequenceState {
  bool has{};
  std::uint64_t epoch{};
  std::uint64_t sequence{};
  std::uint64_t generation{};
};

struct LiveSample {
  std::uint64_t wall_ns{};
  double raw_bps{};
  double gated_bps{};
  double raw_min{};
  double raw_max{};
  double gated_min{};
  double gated_max{};
  std::uint64_t window_samples{};
};

struct Market {
  CatalogMarket catalog;
  BboView bbo{};
  BookSnapshot book{};
  FairPricePod fair{};
  ModelState ewma{};
  bool has_ewma{};
  SequenceState bbo_seq{};
  SequenceState book_seq{};
  std::shared_ptr<const std::string> fair_json;
  std::uint64_t fair_json_generation{static_cast<std::uint64_t>(-1)};
  std::vector<LiveSample> samples;
  std::size_t sample_next{};
  bool sample_full{};
  std::uint64_t sample_hour{};
};

struct MarketView {
  CatalogMarket catalog;
  BboView bbo{};
  BookSnapshot book{};
  FairPricePod fair{};
  std::shared_ptr<const std::string> fair_json;
};

struct RestoreResult {
  bool restored{};
  bool fresh{};
  bool ready{};
  std::uint64_t epoch{};
  std::uint64_t sequence{};
  std::uint64_t generation{};
  FairPricePod fair{};
};

BookSnapshot book_snapshot_from_record(
    const utils::md::wire::AggOrderBookRecord &record);

class Store {
 public:
  explicit Store(std::optional<Engine> engine, std::uint64_t stale_after_ns);

  void reconcile(const CatalogSnapshot &catalog);
  std::string apply_bbo(Market &market, std::uint64_t epoch, std::uint64_t sequence,
                        std::uint64_t generation, std::uint64_t wall_ns,
                        const utils::md::wire::AggBboRecord &record);
  std::string apply_book(Market &market, std::uint64_t epoch, std::uint64_t sequence,
                         std::uint64_t generation, std::uint64_t wall_ns,
                         const BookSnapshot &book, ComputeStats *stats = nullptr);
  std::string apply_bbo_key(const std::string &key, std::uint64_t epoch,
                            std::uint64_t sequence, std::uint64_t generation,
                            std::uint64_t wall_ns,
                            const utils::md::wire::AggBboRecord &record,
                            FairPricePod *published);
  std::string apply_book_key(const std::string &key, std::uint64_t epoch,
                             std::uint64_t sequence, std::uint64_t generation,
                             std::uint64_t wall_ns, const BookSnapshot &book,
                             FairPricePod *published,
                             ComputeStats *stats = nullptr);
  RestoreResult restore_bbo_if_empty(const std::string &key, std::uint64_t epoch,
                                     std::uint64_t sequence,
                                     std::uint64_t generation,
                                     std::uint64_t wall_ns,
                                     const utils::md::wire::AggBboRecord &record,
                                     std::uint64_t now_ns);
  RestoreResult restore_book_if_empty(const std::string &key, std::uint64_t epoch,
                                      std::uint64_t sequence,
                                      std::uint64_t generation,
                                      std::uint64_t wall_ns,
                                      const BookSnapshot &book,
                                      std::uint64_t now_ns);
  [[nodiscard]] bool book_has_live_sequence(const std::string &key) const;
  [[nodiscard]] bool should_discard_startup_publish(
      const std::string &key, std::uint64_t epoch, std::uint64_t sequence,
      std::uint64_t generation) const;
  void mark_fair_not_ready(Market &market, std::string_view reason,
                           std::uint64_t epoch, std::uint64_t sequence,
                           std::uint64_t generation);
  void mark_bbo_not_ready(Market &market, std::string_view reason,
                          std::uint64_t epoch, std::uint64_t sequence,
                          std::uint64_t generation);
  void mark_book_not_ready(Market &market, std::string_view reason,
                           std::uint64_t epoch, std::uint64_t sequence,
                           std::uint64_t generation);
  bool reset_bbo(const std::string &key, std::string_view reason,
                 std::uint64_t epoch, std::uint64_t sequence,
                 std::uint64_t generation, FairPricePod *published);
  bool reset_book(const std::string &key, std::string_view reason,
                  std::uint64_t epoch, std::uint64_t sequence,
                  std::uint64_t generation, FairPricePod *published);
  bool mark_stale(const std::string &key, std::uint64_t now_ns,
                  FairPricePod *published);
  void refresh_fair_json(Market &market);
  void refresh_fair_json_key(const std::string &key);

  std::optional<MarketView> copy_view(const std::string &key) const;
  std::optional<MarketView> copy_unique_symbol(std::string_view symbol,
                                               std::string &error) const;
  std::optional<MarketView> copy_profile_symbol(std::string_view profile,
                                                std::string_view symbol) const;
  Market *find(std::string_view profile, std::string_view symbol);
  Market *find_by_key(const std::string &key);
  Market *find_unique_symbol(std::string_view symbol, std::string &error);
  std::vector<MarketJson> markets_json(std::uint64_t now_ns) const;
  std::vector<Market *> all_markets();
  std::vector<MarketView> copy_all() const;
  std::vector<LiveSample> copy_samples(const std::string &key) const;
  [[nodiscard]] bool ready() const;
  [[nodiscard]] bool ready(std::uint64_t now_ns) const;
  [[nodiscard]] bool fair_enabled() const noexcept { return bool(engine_); }
  [[nodiscard]] std::string model_id() const;
  [[nodiscard]] bool fresh(std::uint64_t wall_ns, std::uint64_t now_ns) const;
  [[nodiscard]] std::uint64_t sqab_encode_count() const noexcept {
    return sqab_encode_count_.load();
  }

 private:
  static std::string check_sequence(SequenceState &state, std::uint64_t epoch,
                                    std::uint64_t sequence,
                                    std::uint64_t generation);
  MarketView view_of(const Market &market) const;
  MarketJson market_json_locked(const Market &market, std::uint64_t now_ns) const;

  std::optional<Engine> engine_;
  std::uint64_t stale_after_ns_{};
  mutable std::mutex mutex_;
  std::unordered_map<std::string, std::unique_ptr<Market>> markets_;
  std::unordered_map<std::string, std::vector<std::string>> symbol_index_;
  std::atomic<std::uint64_t> sqab_encode_count_{0};
};

double signed_spread_bps(std::int64_t bid, std::int64_t ask);

}  // namespace fairprice

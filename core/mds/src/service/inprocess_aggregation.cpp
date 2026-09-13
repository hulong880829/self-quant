#include "mds/service/inprocess_aggregation.h"

#include "mds/exchange/capabilities.h"
#include "mds/publish/wire_publisher.h"
#include "utils/runtime/timestamp.h"

#include <algorithm>
#include <array>
#include <unistd.h>

namespace mds::service {
namespace {

const api::VenueProfile *profile_for(const api::MdsConfig &config,
                                     utils::md::Venue venue,
                                     api::ProductType product) noexcept {
  for (const auto &profile : config.venues) {
    const auto parsed = exchange::parse_venue(profile.venue);
    if (parsed && *parsed == venue && profile.product == product) {
      return &profile;
    }
  }
  return nullptr;
}

std::uint64_t derived_ttl_us(utils::md::Venue venue,
                             api::ProductType product,
                             AggregateKind kind) noexcept {
  const auto *capabilities = exchange::capabilities(venue, product);
  if (capabilities == nullptr) {
    return 0;
  }
  const auto interval_ms =
      kind == AggregateKind::Bbo
          ? (capabilities->ticker.interval_ms != 0
                 ? capabilities->ticker.interval_ms
                 : capabilities->fastest_top10.interval_ms)
          : capabilities->fastest_top10.interval_ms;
  return static_cast<std::uint64_t>(interval_ms) * 3'000U;
}

bool uses_hyperliquid_usdc(
    const api::AggregateSubscription &subscription,
    utils::md::Venue venue) noexcept {
  return venue == utils::md::Venue::Hyperliquid &&
         subscription.symbol.ends_with("USDT");
}

std::string source_symbol(const api::AggregateSubscription &subscription,
                          utils::md::Venue venue) {
  std::string result = subscription.symbol;
  if (uses_hyperliquid_usdc(subscription, venue)) {
    result.replace(result.size() - 4, 4, "USDC");
  }
  return result;
}

template <std::size_t Size>
std::array<char, Size> fixed_text(std::string_view value) noexcept {
  std::array<char, Size> result{};
  std::copy_n(value.begin(), std::min(value.size(), Size - 1),
              result.begin());
  return result;
}

}  // namespace

InProcessAggregation::Input::Input(
    utils::md::Venue venue_value, std::size_t member_index_value,
    transport::SharedRing &&ring_value,
    transport::ReaderHandle reader_value)
    : venue(venue_value),
      member_index(member_index_value),
      ring(std::move(ring_value)),
      reader(reader_value) {}

InProcessAggregation::InProcessAggregation(
    api::SubscriptionHandle handle, AggregateKind kind,
    api::AggregateSubscription subscription, const api::MdsConfig &config)
    : handle_(handle),
      kind_(kind),
      subscription_(std::move(subscription)),
      config_(&config) {}

InProcessAggregation::~InProcessAggregation() { stop(); }

api::Result<void> InProcessAggregation::start() {
  if (config_ == nullptr) {
    return {.error = api::ErrorCode::NotInitialized};
  }
  inputs_.reserve(subscription_.venues.size() + 1);
  const auto marker = transport::process_start_marker(
      static_cast<std::uint32_t>(::getpid()));
  const auto now = utils::runtime::Timestamp::NowMono();
  const std::string_view stream =
      kind_ == AggregateKind::Bbo ? "ticker" : "orderbook";
  bool needs_fx{};

  for (std::size_t index = 0; index < subscription_.venues.size(); ++index) {
    const auto venue = exchange::parse_venue(subscription_.venues[index]);
    if (!venue) {
      stop();
      return {.error = api::ErrorCode::UnsupportedVenueProduct,
              .message = "aggregate venue is unsupported"};
    }
    const auto *profile =
        profile_for(*config_, *venue, subscription_.product);
    if (profile == nullptr) {
      stop();
      return {.error = api::ErrorCode::UnsupportedVenueProduct,
              .message = "aggregate venue profile was not configured"};
    }

    transport::RingOptions options;
    options.backend = config_->shm.backend;
    options.name = publish::make_publisher_segment_name(
        profile->shm_prefix,
        exchange::segment_profile(*venue, subscription_.product),
        source_symbol(subscription_, *venue), stream);
    options.hugetlbfs_mount = config_->shm.hugetlbfs_mount;
    options.create = false;
    options.allow_hugepage_fallback =
        config_->shm.allow_hugepage_fallback;
    auto opened = transport::SharedRing::open(options);
    if (!opened) {
      stop();
      return {.error = opened.error, .message = std::move(opened.message)};
    }
    auto reader = opened.value.register_reader(marker, now);
    if (!reader) {
      stop();
      return {.error = reader.error, .message = std::move(reader.message)};
    }
    inputs_.emplace_back(*venue, index, std::move(opened.value),
                         reader.value);
    needs_fx = needs_fx || uses_hyperliquid_usdc(subscription_, *venue);
  }

  if (needs_fx) {
    const auto *fx_profile = profile_for(
        *config_, utils::md::Venue::Binance,
        utils::md::ProductType::Spot);
    if (fx_profile == nullptr) {
      stop();
      return {.error = api::ErrorCode::UnsupportedVenueProduct,
              .message =
                  "Hyperliquid USDC conversion requires a Binance Spot profile"};
    }
    transport::RingOptions options;
    options.backend = config_->shm.backend;
    options.name = publish::make_publisher_segment_name(
        fx_profile->shm_prefix, "spot", "USDCUSDT", "ticker");
    options.hugetlbfs_mount = config_->shm.hugetlbfs_mount;
    options.create = false;
    options.allow_hugepage_fallback =
        config_->shm.allow_hugepage_fallback;
    auto opened = transport::SharedRing::open(options);
    if (!opened) {
      stop();
      return {.error = opened.error, .message = std::move(opened.message)};
    }
    auto reader = opened.value.register_reader(marker, now);
    if (!reader) {
      stop();
      return {.error = reader.error, .message = std::move(reader.message)};
    }
    inputs_.emplace_back(utils::md::Venue::Binance,
                         agg::kMaxMembers, std::move(opened.value),
                         reader.value);
    inputs_.back().fx = true;
  }
  state_ = api::SubscriptionState::Pending;
  return {};
}

void InProcessAggregation::stop() noexcept {
  for (auto &input : inputs_) {
    if (input.reader) {
      (void)input.ring.unregister_reader(input.reader);
      input.reader = {};
    }
  }
  inputs_.clear();
  engine_.reset();
  have_latest_ = false;
  if (state_ != api::SubscriptionState::Failed) {
    state_ = api::SubscriptionState::Stopped;
  }
}

void InProcessAggregation::reset_engine() noexcept {
  engine_.reset();
  have_latest_ = false;
  state_ = api::SubscriptionState::Pending;
}

bool InProcessAggregation::ensure_engine() noexcept {
  if (engine_) {
    return true;
  }
  std::array<const utils::md::Instrument *, agg::kMaxMembers> instruments{};
  std::uint8_t price_scale{};
  std::uint8_t quantity_scale{};
  for (const auto &input : inputs_) {
    if (input.fx) {
      continue;
    }
    if (!input.compatible) {
      return false;
    }
    const auto *instrument = input.ingest.instrument();
    if (instrument == nullptr) {
      return false;
    }
    instruments[input.member_index] = instrument;
    price_scale = std::max(price_scale, instrument->price_scale);
    quantity_scale = std::max(quantity_scale, instrument->quantity_scale);
  }
  const utils::md::Instrument *identity{};
  for (std::size_t slot = 0; slot < subscription_.venues.size(); ++slot) {
    if (instruments[slot] != nullptr &&
        !uses_hyperliquid_usdc(subscription_,
                              inputs_[slot].venue)) {
      identity = instruments[slot];
      break;
    }
  }
  if (identity == nullptr) {
    identity = instruments.front();
  }
  if (identity == nullptr) {
    return false;
  }
  const bool target_usdt = subscription_.symbol.ends_with("USDT");
  const auto target_quote =
      target_usdt ? fixed_text<16>("USDT") : identity->quote_asset;
  for (std::size_t slot = 0; slot < subscription_.venues.size(); ++slot) {
    const auto *instrument = instruments[slot];
    if (instrument == nullptr ||
        instrument->base_asset != identity->base_asset ||
        (instrument->quote_asset != target_quote &&
         !(uses_hyperliquid_usdc(subscription_, inputs_[slot].venue) &&
           instrument->quote_asset == fixed_text<16>("USDC")))) {
      state_ = api::SubscriptionState::Failed;
      return false;
    }
  }
  if (!metadata_frozen_) {
    base_asset_ = identity->base_asset;
    quote_asset_ = target_quote;
    price_scale_ = price_scale;
    quantity_scale_ = quantity_scale;
    metadata_frozen_ = true;
  } else if (identity->base_asset != base_asset_ ||
             target_quote != quote_asset_ ||
             price_scale > price_scale_ ||
             quantity_scale > quantity_scale_) {
    state_ = api::SubscriptionState::Failed;
    return false;
  }

  engine_.emplace(agg::EngineConfig{
      .instrument_id = identity->instrument_id,
      .base_asset = base_asset_,
      .quote_asset = quote_asset_,
      .price_scale = price_scale_,
      .quantity_scale = quantity_scale_,
      .cross_skew_observe_only =
          subscription_.cross_skew_observe_only,
      .cross_skew_threshold_us =
          subscription_.cross_skew_threshold_us,
      .fx_ttl_us = subscription_.fx_ttl_us,
      .fx_max_depeg_bps = subscription_.fx_max_depeg_bps});
  for (const auto &input : inputs_) {
    if (input.fx) {
      continue;
    }
    const auto *instrument = instruments[input.member_index];
    const auto ttl_us =
        subscription_.ttl_us != 0
            ? subscription_.ttl_us
            : derived_ttl_us(input.venue, subscription_.product,
                             kind_);
    if (!engine_->add_member(
            {.venue = input.venue,
             .ttl_us = ttl_us,
             .price_scale = instrument->price_scale,
             .quantity_scale = instrument->quantity_scale,
             .convert_usdc_to_usdt =
                 uses_hyperliquid_usdc(subscription_, input.venue)})) {
      state_ = api::SubscriptionState::Failed;
      engine_.reset();
      return false;
    }
  }
  for (auto &input : inputs_) {
    if (input.fx) {
      continue;
    }
    if (kind_ == AggregateKind::Bbo) {
      if (const auto *bbo = input.ingest.bbo()) {
        (void)engine_->update_bbo(input.member_index, *bbo,
                                  input.ingest.bbo_ingress_ns());
      }
    } else {
      agg::BookInput book;
      if (input.ingest.book_input(book)) {
        (void)engine_->update_book(input.member_index, book);
      }
    }
  }
  for (const auto &input : inputs_) {
    if (!input.fx) {
      continue;
    }
    const auto *instrument = input.ingest.instrument();
    const auto *bbo = input.ingest.bbo();
    if (instrument != nullptr && bbo != nullptr &&
        instrument->venue == utils::md::Venue::Binance &&
        instrument->product_type == utils::md::ProductType::Spot &&
        instrument->base_asset == fixed_text<16>("USDC") &&
        instrument->quote_asset == fixed_text<16>("USDT")) {
      engine_->update_fx(*bbo, instrument->price_scale,
                         input.ingest.bbo_ingress_ns(),
                         utils::md::Venue::Binance);
    }
  }
  return true;
}

bool InProcessAggregation::poll(std::uint64_t now_mono_ns) noexcept {
  if (state_ == api::SubscriptionState::Failed ||
      state_ == api::SubscriptionState::Stopped) {
    return false;
  }
  bool handled = false;
  for (auto &input : inputs_) {
    for (;;) {
      transport::ReadLease lease;
      const auto error = input.ring.try_read(input.reader, lease);
      if (error == api::ErrorCode::QuotaExceeded) {
        break;
      }
      if (error != api::ErrorCode::Ok) {
        if (error == api::ErrorCode::RecordOverwritten ||
            error == api::ErrorCode::SubscriptionRejected) {
          (void)input.ring.resync_to_latest(input.reader);
          input.ingest.reset();
          reset_engine();
        } else {
          state_ = api::SubscriptionState::Failed;
        }
        break;
      }
      const auto view = lease.view();
      const auto result = input.ingest.consume(
          view.sequence, view.type, view.payload, now_mono_ns);
      (void)lease.commit();
      handled = true;
      if (result == agg::IngestResult::NeedResync ||
          result == agg::IngestResult::Invalid) {
        input.ingest.reset();
        reset_engine();
        continue;
      }
      if (result == agg::IngestResult::Instrument) {
        const auto *instrument = input.ingest.instrument();
        if (!engine_) {
          (void)ensure_engine();
          continue;
        }
        if (input.fx) {
          engine_->invalidate_fx();
          input.compatible =
              instrument != nullptr &&
              instrument->venue == utils::md::Venue::Binance &&
              instrument->product_type ==
                  utils::md::ProductType::Spot &&
              instrument->base_asset == fixed_text<16>("USDC") &&
              instrument->quote_asset == fixed_text<16>("USDT");
          continue;
        }
        const bool expected_usdc =
            uses_hyperliquid_usdc(subscription_, input.venue);
        const auto expected_quote =
            expected_usdc ? fixed_text<16>("USDC") : quote_asset_;
        const auto ttl_us =
            subscription_.ttl_us != 0
                ? subscription_.ttl_us
                : derived_ttl_us(input.venue, subscription_.product,
                                 kind_);
        const bool metadata_valid =
            instrument != nullptr &&
            instrument->venue == input.venue &&
            instrument->product_type == subscription_.product &&
            instrument->base_asset == base_asset_ &&
            instrument->quote_asset == expected_quote &&
            instrument->price_scale <= price_scale_ &&
            instrument->quantity_scale <= quantity_scale_;
        if (!metadata_valid) {
          input.compatible = false;
          engine_->exclude_member(input.member_index);
        } else {
          input.compatible = engine_->reconfigure_member(
                input.member_index,
                {.venue = input.venue,
                 .ttl_us = ttl_us,
                 .price_scale = instrument->price_scale,
                 .quantity_scale = instrument->quantity_scale,
                 .convert_usdc_to_usdt = expected_usdc});
        }
        continue;
      }
      if (!ensure_engine()) {
        continue;
      }
      if (!input.compatible) {
        continue;
      }
      if (input.fx) {
        const auto *instrument = input.ingest.instrument();
        const auto *bbo = input.ingest.bbo();
        if (instrument != nullptr && bbo != nullptr) {
          engine_->update_fx(*bbo, instrument->price_scale,
                             input.ingest.bbo_ingress_ns(),
                             utils::md::Venue::Binance);
        }
        continue;
      }
      if (kind_ == AggregateKind::Bbo) {
        if (const auto *bbo = input.ingest.bbo()) {
          (void)engine_->update_bbo(input.member_index, *bbo,
                                    input.ingest.bbo_ingress_ns());
        }
      } else {
        agg::BookInput book;
        if (input.ingest.book_input(book)) {
          (void)engine_->update_book(input.member_index, book);
        }
      }
    }
  }

  if (!engine_) {
    return handled;
  }
  if (kind_ == AggregateKind::Bbo) {
    const auto built = engine_->build_bbo(now_mono_ns);
    if (built.changed) {
      latest_bbo_ = built.record;
      have_latest_ = true;
      state_ = api::SubscriptionState::Live;
    }
  } else {
    const auto built = engine_->build_orderbook(now_mono_ns);
    if (built.changed && built.publishable) {
      latest_book_ = built.record;
      have_latest_ = true;
      state_ = api::SubscriptionState::Live;
    }
  }
  return handled;
}

api::ErrorCode
InProcessAggregation::read(api::AggBboRecord &record) const noexcept {
  if (kind_ != AggregateKind::Bbo) {
    return api::ErrorCode::SubscriptionTypeMismatch;
  }
  if (!have_latest_) {
    return state_ == api::SubscriptionState::Failed
               ? api::ErrorCode::InstrumentMismatch
               : api::ErrorCode::AggregateNotReady;
  }
  record = latest_bbo_;
  return api::ErrorCode::Ok;
}

api::ErrorCode InProcessAggregation::read(
    api::AggOrderBookRecord &record) const noexcept {
  if (kind_ != AggregateKind::OrderBook) {
    return api::ErrorCode::SubscriptionTypeMismatch;
  }
  if (!have_latest_) {
    return state_ == api::SubscriptionState::Failed
               ? api::ErrorCode::InstrumentMismatch
               : api::ErrorCode::AggregateNotReady;
  }
  record = latest_book_;
  return api::ErrorCode::Ok;
}

}  // namespace mds::service

#include "mds/agg/venue_ingest.h"

#include <algorithm>
#include <string_view>

namespace mds::agg {

VenueIngest::VenueIngest(std::size_t ladder_capacity)
    : ladder_capacity_(
          std::min(ladder_capacity, utils::md::kMaxLadderLevels)),
      book_(std::make_unique<utils::md::OrderBook>(ladder_capacity_)) {}

VenueIngest::VenueIngest(Selector selector, std::size_t ladder_capacity)
    : VenueIngest(ladder_capacity) {
  selector_ = std::move(selector);
}

namespace {
template <std::size_t Size>
std::string_view fixed_view(const std::array<char, Size> &value) noexcept {
  const auto end = std::find(value.begin(), value.end(), '\0');
  return {value.data(), static_cast<std::size_t>(end - value.begin())};
}
}  // namespace

bool VenueIngest::sequence_ok(
    std::uint64_t ring_sequence,
    const utils::md::wire::RecordHeader &header) noexcept {
  if (have_sequence_ &&
      (ring_sequence != last_ring_sequence_ + 1 ||
       header.bus_seq != last_bus_sequence_ + 1)) {
    return false;
  }
  const bool instrument =
      header.message_type == static_cast<std::uint16_t>(
                                 utils::md::MessageType::InstrumentUpdate) ||
      header.message_type == static_cast<std::uint16_t>(
                                 utils::md::MessageType::InstrumentCatalog);
  return instrument || header.instrument_id != selected_instrument_id_ ||
         !have_source_sequence_ ||
         header.book_generation != source_generation_ ||
         header.source_seq >= last_source_sequence_;
}

void VenueIngest::accept_sequence(
    std::uint64_t ring_sequence,
    const utils::md::wire::RecordHeader &header) noexcept {
  last_ring_sequence_ = ring_sequence;
  last_bus_sequence_ = header.bus_seq;
  have_sequence_ = true;
  if (header.instrument_id == selected_instrument_id_ &&
      header.message_type != static_cast<std::uint16_t>(
                                 utils::md::MessageType::InstrumentUpdate) &&
      header.message_type != static_cast<std::uint16_t>(
                                 utils::md::MessageType::InstrumentCatalog)) {
    last_source_sequence_ = header.source_seq;
    source_generation_ = header.book_generation;
    have_source_sequence_ = true;
  }
}

IngestResult VenueIngest::consume(
    std::uint64_t ring_sequence, std::uint32_t outer_type,
    std::span<const std::byte> payload,
    std::uint64_t ingress_mono_ns) noexcept {
  utils::md::wire::RecordHeader header{};
  const auto validation =
      utils::md::wire::ValidateHeader(payload, &header);
  if (validation == utils::md::wire::CodecError::UnknownMessageType) {
    if (outer_type != header.message_type ||
        (have_sequence_ &&
         (ring_sequence != last_ring_sequence_ + 1 ||
          header.bus_seq != last_bus_sequence_ + 1))) {
      reset();
      return IngestResult::NeedResync;
    }
    last_ring_sequence_ = ring_sequence;
    last_bus_sequence_ = header.bus_seq;
    have_sequence_ = true;
    return IngestResult::Ignored;
  }
  if (validation != utils::md::wire::CodecError::Ok ||
      outer_type != header.message_type ||
      !sequence_ok(ring_sequence, header)) {
    reset();
    return IngestResult::NeedResync;
  }

  const auto type = static_cast<utils::md::MessageType>(header.message_type);
  if (type != utils::md::MessageType::InstrumentCatalog &&
      type != utils::md::MessageType::InstrumentUpdate &&
      (selected_instrument_id_ == 0 ||
       header.instrument_id != selected_instrument_id_)) {
    accept_sequence(ring_sequence, header);
    return IngestResult::Ignored;
  }
  switch (type) {
  case utils::md::MessageType::InstrumentCatalog: {
    utils::md::wire::InstrumentCatalogRecord record{};
    if (utils::md::wire::DecodeInstrumentCatalog(payload, record) !=
        utils::md::wire::CodecError::Ok) {
      return IngestResult::Invalid;
    }
    const bool selector_matches =
        (selector_.venue == utils::md::Venue::Unknown ||
         record.catalog.venue == selector_.venue) &&
        (selector_.product == utils::md::ProductType::Unknown ||
         record.catalog.product_type == selector_.product) &&
        (selector_.canonical_symbol.empty() ||
         fixed_view(record.catalog.canonical_symbol) ==
             selector_.canonical_symbol);
    if (selected_instrument_id_ == 0 && selector_matches) {
      selected_instrument_id_ = record.header.instrument_id;
    }
    accept_sequence(ring_sequence, header);
    return IngestResult::Ignored;
  }
  case utils::md::MessageType::InstrumentUpdate: {
    utils::md::wire::InstrumentUpdateRecord record{};
    if (utils::md::wire::DecodeInstrument(payload, record) !=
        utils::md::wire::CodecError::Ok) {
      return IngestResult::Invalid;
    }
    const bool selector_matches =
        (selector_.venue == utils::md::Venue::Unknown ||
         record.instrument.venue == selector_.venue) &&
        (selector_.product == utils::md::ProductType::Unknown ||
         record.instrument.product_type == selector_.product) &&
        (selector_.canonical_symbol.empty() ||
         fixed_view(record.instrument.canonical_symbol) ==
             selector_.canonical_symbol);
    if (selected_instrument_id_ == 0 && selector_matches) {
      selected_instrument_id_ = record.header.instrument_id;
    }
    if (record.header.instrument_id != selected_instrument_id_) {
      accept_sequence(ring_sequence, header);
      return IngestResult::Ignored;
    }
    const bool changed =
        have_instrument_ &&
        (instrument_.instrument_id != record.instrument.instrument_id ||
         instrument_.venue != record.instrument.venue ||
         instrument_.product_type != record.instrument.product_type ||
         instrument_.price_scale != record.instrument.price_scale ||
         instrument_.quantity_scale != record.instrument.quantity_scale ||
         instrument_.contract_multiplier_scale !=
             record.instrument.contract_multiplier_scale ||
         instrument_.flags != record.instrument.flags ||
         instrument_.tick_size != record.instrument.tick_size ||
         instrument_.lot_size != record.instrument.lot_size ||
         instrument_.contract_multiplier !=
             record.instrument.contract_multiplier ||
         instrument_.base_asset != record.instrument.base_asset ||
         instrument_.quote_asset != record.instrument.quote_asset ||
         instrument_.settle_asset != record.instrument.settle_asset ||
         instrument_.canonical_symbol !=
             record.instrument.canonical_symbol ||
         instrument_.venue_symbol != record.instrument.venue_symbol);
    instrument_changed_ = changed;
    instrument_ = record.instrument;
    have_instrument_ = instrument_.tick_size > 0;
    if (changed) {
      have_bbo_ = false;
      snapshot_active_ = false;
      bridge_.reset();
      book_->Reset(0, 0, 0, header.book_generation);
      ++book_generation_;
    }
    if (have_instrument_ && !bridge_) {
      bridge_.emplace(
          *book_, instrument_.tick_size,
          (instrument_.flags & utils::md::kInstrumentRefineBookTick) != 0);
    }
    accept_sequence(ring_sequence, header);
    return have_instrument_ ? IngestResult::Instrument
                            : IngestResult::Invalid;
  }
  case utils::md::MessageType::Bbo:
  case utils::md::MessageType::Ticker: {
    utils::md::wire::BboRecord record{};
    utils::md::wire::CodecError decoded{};
    if (type == utils::md::MessageType::Bbo) {
      decoded = utils::md::wire::DecodeBbo(payload, record);
    } else {
      utils::md::wire::TickerRecord ticker{};
      decoded = utils::md::wire::DecodeTicker(payload, ticker);
      if (decoded == utils::md::wire::CodecError::Ok) {
        record.header = ticker.header;
        record.bid_price = ticker.bid_price;
        record.bid_quantity = ticker.bid_quantity;
        record.ask_price = ticker.ask_price;
        record.ask_quantity = ticker.ask_quantity;
      }
    }
    if (decoded != utils::md::wire::CodecError::Ok) {
      return IngestResult::Invalid;
    }
    bbo_ = record;
    bbo_ingress_ns_ = ingress_mono_ns;
    have_bbo_ = true;
    accept_sequence(ring_sequence, header);
    return IngestResult::Bbo;
  }
  case utils::md::MessageType::SnapshotBegin: {
    utils::md::wire::SnapshotBeginRecord record{};
    if (!bridge_ ||
        utils::md::wire::DecodeSnapshotBegin(payload, record) !=
            utils::md::wire::CodecError::Ok ||
        record.item_count > snapshot_bids_.size() + snapshot_asks_.size()) {
      return IngestResult::Invalid;
    }
    snapshot_bid_count_ = 0;
    snapshot_ask_count_ = 0;
    snapshot_bridge_sequence_ = record.header.bus_seq;
    snapshot_active_ = true;
    accept_sequence(ring_sequence, header);
    return IngestResult::Ignored;
  }
  case utils::md::MessageType::SnapshotChunk: {
    utils::md::wire::SnapshotChunkRecord record{};
    if (!snapshot_active_ ||
        utils::md::wire::DecodeSnapshotChunk(payload, record) !=
            utils::md::wire::CodecError::Ok) {
      return IngestResult::Invalid;
    }
    auto *destination = record.side == static_cast<std::uint8_t>(
                                           utils::md::Side::Bid)
                            ? snapshot_bids_.data()
                            : snapshot_asks_.data();
    auto &count = record.side == static_cast<std::uint8_t>(
                                      utils::md::Side::Bid)
                      ? snapshot_bid_count_
                      : snapshot_ask_count_;
    if (count + record.level_count > utils::md::kMaxLadderLevels) {
      snapshot_active_ = false;
      return IngestResult::Invalid;
    }
    std::copy_n(record.levels.begin(), record.level_count,
                destination + count);
    count += record.level_count;
    accept_sequence(ring_sequence, header);
    return IngestResult::Ignored;
  }
  case utils::md::MessageType::SnapshotEnd: {
    utils::md::wire::SnapshotEndRecord record{};
    if (!snapshot_active_ ||
        utils::md::wire::DecodeSnapshotEnd(payload, record) !=
            utils::md::wire::CodecError::Ok ||
        !rebuild_book(record)) {
      snapshot_active_ = false;
      return IngestResult::NeedResync;
    }
    snapshot_active_ = false;
    book_ingress_ns_ = ingress_mono_ns;
    book_exchange_ts_ns_ = header.exchange_ts_ns;
    book_dirty_ = true;
    accept_sequence(ring_sequence, header);
    return IngestResult::Book;
  }
  case utils::md::MessageType::BookDelta: {
    utils::md::wire::DeltaRecord record{};
    if (!bridge_ ||
        utils::md::wire::DecodeDelta(payload, record) !=
            utils::md::wire::CodecError::Ok) {
      return IngestResult::Invalid;
    }
    const utils::md::Level level{record.price, record.quantity};
    const auto side = static_cast<utils::md::Side>(record.side);
    const std::span<const utils::md::Level> bids =
        side == utils::md::Side::Bid
            ? std::span<const utils::md::Level>(&level, 1)
            : std::span<const utils::md::Level>();
    const std::span<const utils::md::Level> asks =
        side == utils::md::Side::Ask
            ? std::span<const utils::md::Level>(&level, 1)
            : std::span<const utils::md::Level>();
    if (bridge_->Apply({record.header.bus_seq, record.header.bus_seq,
                        bids, asks}) != book::BridgeAction::Applied) {
      return IngestResult::NeedResync;
    }
    book_ingress_ns_ = ingress_mono_ns;
    book_exchange_ts_ns_ = header.exchange_ts_ns;
    book_dirty_ = true;
    accept_sequence(ring_sequence, header);
    return IngestResult::Book;
  }
  case utils::md::MessageType::AggBbo:
  case utils::md::MessageType::AggOrderBook:
    return IngestResult::Ignored;
  }
  return IngestResult::Invalid;
}

bool VenueIngest::rebuild_book(
    const utils::md::wire::SnapshotEndRecord &end) noexcept {
  if (!bridge_ ||
      end.item_count != snapshot_bid_count_ + snapshot_ask_count_) {
    return false;
  }
  const auto loaded = bridge_->LoadSnapshot(
      {snapshot_bridge_sequence_,
       std::span<const utils::md::Level>(snapshot_bids_.data(),
                                         snapshot_bid_count_),
       std::span<const utils::md::Level>(snapshot_asks_.data(),
                                         snapshot_ask_count_)},
      end.header.book_generation);
  if (loaded.action != book::BridgeAction::Applied) {
    return false;
  }
  bridge_->SetLive();
  book_generation_ = end.header.book_generation;
  return true;
}

bool VenueIngest::book_input(BookInput &input) noexcept {
  if (!book_dirty_ || book_->state() != utils::md::BookState::Live) {
    return false;
  }
  top_bid_count_ = 0;
  top_ask_count_ = 0;
  const auto collect = [](const utils::md::Ladder &ladder, bool descending,
                          auto &destination, std::size_t &count) {
    auto index = ladder.best_index();
    while (index >= 0 &&
           index < static_cast<std::int32_t>(ladder.capacity()) &&
           count < destination.size()) {
      const auto level = ladder.At(static_cast<std::size_t>(index));
      if (level) {
        destination[count++] = *level;
      }
      index += descending ? -1 : 1;
    }
  };
  collect(book_->bids(), true, top_bids_, top_bid_count_);
  collect(book_->asks(), false, top_asks_, top_ask_count_);
  input = {std::span<const utils::md::Level>(top_bids_.data(), top_bid_count_),
           std::span<const utils::md::Level>(top_asks_.data(), top_ask_count_),
           book_exchange_ts_ns_, book_ingress_ns_, book_generation_};
  book_dirty_ = false;
  return top_bid_count_ != 0 && top_ask_count_ != 0;
}

void VenueIngest::reset() noexcept {
  have_sequence_ = false;
  have_source_sequence_ = false;
  have_bbo_ = false;
  snapshot_active_ = false;
  book_dirty_ = false;
  instrument_changed_ = false;
  if (!selector_.canonical_symbol.empty()) {
    selected_instrument_id_ = 0;
    have_instrument_ = false;
  }
  if (book_) {
    book_->Reset(0, 0, 0, ++book_generation_);
  }
}

}  // namespace mds::agg

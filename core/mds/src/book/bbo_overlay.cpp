#include "mds/book/bbo_overlay.h"

#include <limits>

namespace mds::book {
namespace {

bool valid_bbo(const utils::md::BboEvent &event) noexcept {
  return event.bid.price >= 0 && event.ask.price >= 0 &&
         event.bid.quantity >= 0 && event.ask.quantity >= 0 &&
         event.bid.price < event.ask.price;
}

bool same_bbo(const utils::md::BboEvent &event, const utils::md::Level &bid,
              const utils::md::Level &ask) noexcept {
  return event.bid.price == bid.price &&
         event.bid.quantity == bid.quantity &&
         event.ask.price == ask.price &&
         event.ask.quantity == ask.quantity;
}

}  // namespace

OverlayAction BboOverlay::OnTicker(
    const utils::md::BboEvent &ticker,
    const utils::md::BboEvent &canonical) noexcept {
  if (!valid_bbo(ticker) ||
      ticker.header.instrument_id != canonical.header.instrument_id) {
    return OverlayAction::Ignored;
  }
  if (valid_ != 0 && ticker.header.book_generation == generation_ &&
      ticker.header.source_seq <= sequence_) {
    return OverlayAction::Ignored;
  }

  bid_ = ticker.bid;
  ask_ = ticker.ask;
  sequence_ = ticker.header.source_seq;
  receive_tsc_ = ticker.header.receive_tsc;
  generation_ = ticker.header.book_generation;
  instrument_id_ = ticker.header.instrument_id;
  source_id_ = ticker.header.source_id;
  valid_ = 1;

  if (domain_ == SequenceDomain::Independent) {
    return OverlayAction::TickerOnly;
  }
  if (ticker.header.book_generation != canonical.header.book_generation ||
      ticker.header.source_seq <= canonical.header.source_seq) {
    Clear();
    return OverlayAction::Ignored;
  }
  return OverlayAction::Updated;
}

OverlayAction BboOverlay::OnCanonical(
    const utils::md::BboEvent &canonical) noexcept {
  if (domain_ == SequenceDomain::Independent || valid_ == 0) {
    return OverlayAction::Ignored;
  }
  if (canonical.header.instrument_id != instrument_id_ ||
      canonical.header.book_generation != generation_) {
    Clear();
    return OverlayAction::Cleared;
  }
  if (canonical.header.source_seq < sequence_) {
    return OverlayAction::Pending;
  }
  // If depth advanced past the ticker sequence, that older BBO can no longer
  // be compared with the newer canonical state. It is stale, not divergent.
  if (canonical.header.source_seq > sequence_) {
    Clear();
    return OverlayAction::Cleared;
  }
  if (same_bbo(canonical, bid_, ask_)) {
    Clear();
    return OverlayAction::Cleared;
  }
  if (divergence_count_ != std::numeric_limits<std::uint32_t>::max()) {
    ++divergence_count_;
  }
  valid_ = 0;
  return OverlayAction::Divergence;
}

std::optional<utils::md::BboEvent> BboOverlay::Effective(
    const utils::md::BboEvent &canonical) const noexcept {
  if (domain_ != SequenceDomain::Shared || valid_ == 0 ||
      generation_ != canonical.header.book_generation ||
      instrument_id_ != canonical.header.instrument_id ||
      sequence_ <= canonical.header.source_seq) {
    return valid_bbo(canonical)
               ? std::optional<utils::md::BboEvent>{canonical}
               : std::nullopt;
  }
  auto effective = canonical;
  effective.header.source_seq = sequence_;
  effective.header.receive_tsc = receive_tsc_;
  effective.header.source_id = source_id_;
  effective.bid = bid_;
  effective.ask = ask_;
  return effective;
}

std::optional<utils::md::BboEvent> BboOverlay::ticker() const noexcept {
  if (valid_ == 0) {
    return std::nullopt;
  }
  utils::md::BboEvent event{};
  event.header.instrument_id = instrument_id_;
  event.header.book_generation = generation_;
  event.header.source_seq = sequence_;
  event.header.receive_tsc = receive_tsc_;
  event.header.source_id = source_id_;
  event.header.state = utils::md::BookState::Live;
  event.bid = bid_;
  event.ask = ask_;
  return event;
}

void BboOverlay::Clear() noexcept {
  bid_ = {};
  ask_ = {};
  sequence_ = 0;
  receive_tsc_ = 0;
  generation_ = 0;
  instrument_id_ = 0;
  source_id_ = 0;
  valid_ = 0;
}

}  // namespace mds::book

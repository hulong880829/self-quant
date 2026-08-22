#include "mds/consume/aggregate_dispatch.h"

#include <algorithm>

namespace mds::consume {

void AggregateLatestState::publish(
    const utils::md::wire::AggBboRecord &record,
    AggregateReceiveInfo receive) noexcept {
  service_bbo_window_rollover(receive);
  bbo_writer_.record = record;
  bbo_writer_.status.receive = receive;
  bbo_writer_.status.ready = true;
  auto &window = bbo_writer_.cross_window;
  if (!bbo_window_initialized_) {
    window.start_mono_ns = receive.receive_mono_ns;
    window.end_mono_ns = receive.receive_mono_ns;
    window.raw_min = record.raw_cross_bps;
    window.raw_max = record.raw_cross_bps;
    window.gated_min = record.gated_cross_bps;
    window.gated_max = record.gated_cross_bps;
    window.sample_count = 1;
    bbo_window_initialized_ = true;
  } else {
    window.end_mono_ns = receive.receive_mono_ns;
    window.raw_min = std::min(window.raw_min, record.raw_cross_bps);
    window.raw_max = std::max(window.raw_max, record.raw_cross_bps);
    window.gated_min = std::min(window.gated_min, record.gated_cross_bps);
    window.gated_max = std::max(window.gated_max, record.gated_cross_bps);
    ++window.sample_count;
  }
  (void)bbo_.publish(bbo_writer_);
}

void AggregateLatestState::publish(
    const utils::md::wire::AggOrderBookRecord &record,
    AggregateReceiveInfo receive) noexcept {
  book_writer_.record = record;
  book_writer_.status.receive = receive;
  book_writer_.status.ready = true;
  (void)book_.publish(book_writer_);
}

void AggregateLatestState::reset(AggregateTopic topic,
                                 AggregateReceiveInfo receive) noexcept {
  switch (topic) {
    case AggregateTopic::AggBbo:
      bbo_writer_ = {};
      bbo_writer_.status.receive = receive;
      bbo_window_initialized_ = false;
      (void)bbo_.publish(bbo_writer_);
      bbo_window_writer_ = {};
      bbo_window_writer_.status.receive = receive;
      served_rollover_ =
          requested_rollover_.load(std::memory_order_acquire);
      bbo_window_writer_.rollover_request = served_rollover_;
      (void)bbo_windows_.publish(bbo_window_writer_);
      break;
    case AggregateTopic::AggOrderBook:
      book_writer_ = {};
      book_writer_.status.receive = receive;
      (void)book_.publish(book_writer_);
      break;
    case AggregateTopic::Unsupported:
      break;
  }
}

std::uint64_t
AggregateLatestState::request_bbo_window_rollover() noexcept {
  return requested_rollover_.fetch_add(1, std::memory_order_acq_rel) + 1;
}

void AggregateLatestState::service_bbo_window_rollover(
    AggregateReceiveInfo receive) noexcept {
  const auto requested =
      requested_rollover_.load(std::memory_order_acquire);
  const bool request_due = requested != served_rollover_;
  const auto start = bbo_writer_.cross_window.start_mono_ns;
  const bool interval_due =
      bbo_window_initialized_ && bbo_window_interval_ns_ != 0 &&
      receive.receive_mono_ns >= start &&
      receive.receive_mono_ns - start >= bbo_window_interval_ns_;
  if (!request_due && !interval_due) {
    return;
  }
  if (interval_due) {
    receive.receive_mono_ns = start + bbo_window_interval_ns_;
  }
  complete_bbo_window(receive, requested);
  served_rollover_ = requested;
}

void AggregateLatestState::complete_bbo_window(
    AggregateReceiveInfo receive, std::uint64_t rollover_request) noexcept {
  auto completed = bbo_writer_.cross_window;
  if (completed.sample_count == 0) {
    completed.start_mono_ns = receive.receive_mono_ns;
  }
  completed.end_mono_ns = receive.receive_mono_ns;

  bbo_writer_.cross_window = {};
  bbo_window_initialized_ = false;
  if (bbo_writer_.status.ready) {
    bbo_writer_.status.receive = receive;
    (void)bbo_.publish(bbo_writer_);
  }

  bbo_window_writer_.status.receive = receive;
  bbo_window_writer_.status.ready = true;
  bbo_window_writer_.cross_window = completed;
  bbo_window_writer_.rollover_request = rollover_request;
  (void)bbo_windows_.publish(bbo_window_writer_);
}

AggregateTopic
aggregate_topic_from_segment(std::string_view segment) noexcept {
  if (segment.ends_with(".aggbbo.2")) {
    return AggregateTopic::AggBbo;
  }
  if (segment.ends_with(".aggorderbook.2")) {
    return AggregateTopic::AggOrderBook;
  }
  return AggregateTopic::Unsupported;
}

}  // namespace mds::consume

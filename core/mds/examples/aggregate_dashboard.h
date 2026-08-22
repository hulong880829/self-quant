#pragma once

#include "utils/md/wire.h"

#include <cstddef>
#include <cstdint>
#include <string>
#include <string_view>

namespace mds::examples {

struct AggregateDashboardView {
  const utils::md::wire::AggBboRecord *bbo{};
  const utils::md::wire::AggOrderBookRecord *book{};
  std::string_view bbo_segment;
  std::string_view book_segment;
  std::uint64_t bbo_ring_sequence{};
  std::uint64_t book_ring_sequence{};
  std::size_t depth{10};
  std::size_t terminal_width{120};
  bool color{true};
  bool expect_bbo{true};
  bool expect_book{true};
};

[[nodiscard]] std::string render_aggregate_dashboard(
    const AggregateDashboardView &view);

}  // namespace mds::examples

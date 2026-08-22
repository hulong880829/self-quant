#include "mds/record/record_reader.h"

#include <filesystem>
#include <iostream>
#include <string_view>
#include <vector>

namespace {

std::string_view kind_name(mds::record::Kind kind) {
  return kind == mds::record::Kind::AggBbo ? "aggbbo" : "aggorderbook";
}

}  // namespace

int main(int argc, char **argv) {
  bool export_json = false;
  std::vector<std::filesystem::path> paths;
  for (int index = 1; index < argc; ++index) {
    const std::string_view argument(argv[index]);
    if (argument == "--export-jsonl") {
      export_json = true;
    } else if (argument == "--validate") {
      export_json = false;
    } else if (!argument.empty() && argument.front() != '-') {
      paths.emplace_back(argument);
    } else {
      std::cerr << "usage: mds_record_tool [--validate|--export-jsonl] "
                   "<shard> [shard ...]\n";
      return 2;
    }
  }
  if (paths.empty()) {
    std::cerr << "usage: mds_record_tool [--validate|--export-jsonl] "
                 "<shard> [shard ...]\n";
    return 2;
  }

  for (const auto &path : paths) {
    mds::record::RecordVisitor visitor;
    if (export_json) {
      visitor = [](const auto &record) {
          std::cout << "{\"kind\":\"" << kind_name(record.metadata.kind)
                    << "\",\"wall_ns\":" << record.metadata.wall_ns
                    << ",\"mono_ns\":" << record.metadata.mono_ns
                    << ",\"ring_epoch\":" << record.metadata.ring_epoch
                    << ",\"ring_sequence\":"
                    << record.metadata.ring_sequence
                    << ",\"generation\":" << record.metadata.generation
                    << ",\"flags\":" << record.metadata.flags;
          if (record.metadata.kind == mds::record::Kind::AggBbo) {
            std::cout << ",\"raw_cross_bps\":"
                      << record.bbo.raw_cross_bps
                      << ",\"gated_cross_bps\":"
                      << record.bbo.gated_cross_bps
                      << ",\"raw_min\":" << record.cross_window.raw_min
                      << ",\"raw_max\":" << record.cross_window.raw_max
                      << ",\"gated_min\":"
                      << record.cross_window.gated_min
                      << ",\"gated_max\":"
                      << record.cross_window.gated_max
                      << ",\"window_start_mono_ns\":"
                      << record.cross_window.start_mono_ns
                      << ",\"window_end_mono_ns\":"
                      << record.cross_window.end_mono_ns
                      << ",\"samples\":" << record.cross_window.samples;
          } else {
            std::cout << ",\"bid_count\":"
                      << record.order_book.bid_count
                      << ",\"ask_count\":"
                      << record.order_book.ask_count << ",\"bids\":[";
            for (std::size_t index = 0;
                 index < record.order_book.bid_count; ++index) {
              const auto &level = record.order_book.bids[index];
              std::cout << (index == 0 ? "" : ",") << "{\"price\":"
                        << level.price << ",\"quantity\":" << level.quantity
                        << ",\"venue_mask\":" << level.venue_mask << '}';
            }
            std::cout << "],\"asks\":[";
            for (std::size_t index = 0;
                 index < record.order_book.ask_count; ++index) {
              const auto &level = record.order_book.asks[index];
              std::cout << (index == 0 ? "" : ",") << "{\"price\":"
                        << level.price << ",\"quantity\":" << level.quantity
                        << ",\"venue_mask\":" << level.venue_mask << '}';
            }
            std::cout << ']';
          }
          std::cout << "}\n";
          return true;
      };
    }
    const auto result = mds::record::read_file(path, visitor);
    if (!result) {
      std::cerr << path << ": " << result.message << '\n';
      return 1;
    }
    if (!export_json) {
      std::cout << path << ": valid records=" << result.records << '\n';
    }
  }
  return 0;
}

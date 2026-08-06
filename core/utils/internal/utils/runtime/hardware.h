#pragma once

#include <pthread.h>

#include <cstdint>
#include <string>
#include <string_view>
#include <vector>

namespace utils::runtime {

struct CpuInfo {
  std::uint32_t id{};
  std::int32_t numa_node{-1};
  std::int32_t core_id{-1};
  bool online{};
  std::vector<std::uint32_t> thread_siblings;
};

struct NicInfo {
  std::string name;
  std::int32_t numa_node{-1};
  std::uint32_t rx_queues{};
  std::uint32_t tx_queues{};
};

struct HardwareTopology {
  std::vector<CpuInfo> cpus;
  std::vector<NicInfo> nics;
  bool smt_enabled{};
  std::vector<std::string> warnings;
};

enum class ThreadRole : std::uint8_t { Network, Parser, Book, Publisher, Control };
struct ThreadAssignment { ThreadRole role{}; std::uint32_t cpu{}; std::int32_t numa_node{-1}; };
struct RuntimePlan {
  std::vector<ThreadAssignment> assignments;
  std::vector<std::string> warnings;
  bool valid{};
};
struct ManualAssignment { ThreadRole role{}; std::uint32_t cpu{}; };

[[nodiscard]] HardwareTopology DetectHardware(
    std::string_view sys_root = "/sys") noexcept;
[[nodiscard]] RuntimePlan MakeAutomaticPlan(
    const HardwareTopology& topology, std::size_t book_threads,
    std::string_view nic = {}) noexcept;
[[nodiscard]] RuntimePlan ValidateManualPlan(
    const HardwareTopology& topology,
    const std::vector<ManualAssignment>& assignments) noexcept;
[[nodiscard]] int BindCpu(pthread_t thread, std::uint32_t cpu) noexcept;
[[nodiscard]] int BindCurrentThread(std::uint32_t cpu) noexcept;

}  // namespace utils::runtime

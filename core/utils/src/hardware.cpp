#include "utils/runtime/hardware.h"

#include <sched.h>

#include <algorithm>
#include <cerrno>
#include <charconv>
#include <filesystem>
#include <fstream>
#include <optional>
#include <set>

namespace utils::runtime {
namespace {
std::optional<std::string> ReadText(const std::filesystem::path& path) {
  std::ifstream input(path);
  std::string value;
  if (!input || !std::getline(input, value)) return std::nullopt;
  return value;
}

std::optional<std::int32_t> ReadInt(const std::filesystem::path& path) {
  const auto text = ReadText(path);
  if (!text) return std::nullopt;
  std::int32_t value{};
  const auto result = std::from_chars(text->data(), text->data() + text->size(), value);
  return result.ec == std::errc{} ? std::optional(value) : std::nullopt;
}

std::vector<std::uint32_t> ParseList(std::string_view text) {
  std::vector<std::uint32_t> values;
  while (!text.empty()) {
    const auto comma = text.find(',');
    const auto token = text.substr(0, comma);
    const auto dash = token.find('-');
    std::uint32_t first{}, last{};
    const auto first_text = token.substr(0, dash);
    if (std::from_chars(first_text.data(), first_text.data() + first_text.size(), first).ec == std::errc{}) {
      last = first;
      if (dash != std::string_view::npos) {
        const auto last_text = token.substr(dash + 1);
        if (std::from_chars(last_text.data(), last_text.data() + last_text.size(), last).ec != std::errc{})
          last = first;
      }
      for (std::uint32_t value = first; value <= last; ++value) values.push_back(value);
    }
    if (comma == std::string_view::npos) break;
    text.remove_prefix(comma + 1);
  }
  return values;
}

std::int32_t CpuNode(const std::filesystem::path& cpu_path) {
  std::error_code error;
  for (const auto& entry : std::filesystem::directory_iterator(cpu_path, error)) {
    const auto name = entry.path().filename().string();
    if (name.starts_with("node")) {
      std::int32_t node{};
      if (std::from_chars(name.data() + 4, name.data() + name.size(), node).ec == std::errc{}) return node;
    }
  }
  return -1;
}

std::uint32_t QueueCount(const std::filesystem::path& path, std::string_view prefix) {
  std::uint32_t count = 0;
  std::error_code error;
  for (const auto& entry : std::filesystem::directory_iterator(path, error))
    if (entry.path().filename().string().starts_with(prefix)) ++count;
  return count;
}
}  // namespace

HardwareTopology DetectHardware(std::string_view sys_root) noexcept {
  HardwareTopology topology;
  try {
    const auto root = std::filesystem::path(sys_root);
    const auto online_text = ReadText(root / "devices/system/cpu/online");
    if (!online_text) {
      topology.warnings.emplace_back("CPU online list unavailable");
      return topology;
    }
    const auto online = ParseList(*online_text);
    for (const auto id : online) {
      const auto cpu_path = root / "devices/system/cpu" / ("cpu" + std::to_string(id));
      CpuInfo cpu{id, CpuNode(cpu_path), -1, true, {}};
      cpu.core_id = ReadInt(cpu_path / "topology/core_id").value_or(-1);
      if (const auto siblings = ReadText(cpu_path / "topology/thread_siblings_list"))
        cpu.thread_siblings = ParseList(*siblings);
      topology.smt_enabled |= cpu.thread_siblings.size() > 1;
      topology.cpus.push_back(std::move(cpu));
    }
    const auto net_path = root / "class/net";
    std::error_code error;
    for (const auto& entry : std::filesystem::directory_iterator(net_path, error)) {
      NicInfo nic;
      nic.name = entry.path().filename().string();
      nic.numa_node = ReadInt(entry.path() / "device/numa_node").value_or(-1);
      nic.rx_queues = QueueCount(entry.path() / "queues", "rx-");
      nic.tx_queues = QueueCount(entry.path() / "queues", "tx-");
      topology.nics.push_back(std::move(nic));
    }
    if (topology.nics.empty()) topology.warnings.emplace_back("NIC topology unavailable");
  } catch (...) {
    topology.warnings.emplace_back("sysfs topology scan failed");
  }
  return topology;
}

RuntimePlan MakeAutomaticPlan(const HardwareTopology& topology, std::size_t book_threads,
                              std::string_view nic_name) noexcept {
  RuntimePlan plan;
  std::int32_t preferred_node = -1;
  if (!nic_name.empty()) {
    const auto nic = std::find_if(topology.nics.begin(), topology.nics.end(),
                                  [&](const NicInfo& item) { return item.name == nic_name; });
    if (nic == topology.nics.end()) {
      plan.warnings.emplace_back("requested NIC not found");
      return plan;
    }
    preferred_node = nic->numa_node;
  }
  std::vector<const CpuInfo*> candidates;
  std::set<std::pair<std::int32_t, std::int32_t>> used_cores;
  for (const auto& cpu : topology.cpus) {
    if (!cpu.online || (preferred_node >= 0 && cpu.numa_node != preferred_node)) continue;
    if (used_cores.emplace(cpu.numa_node, cpu.core_id).second) candidates.push_back(&cpu);
  }
  const std::size_t needed = 4 + book_threads;
  if (candidates.size() < needed) {
    plan.warnings.emplace_back("insufficient independent physical cores");
    return plan;
  }
  std::size_t index = 0;
  const auto add = [&](ThreadRole role, RuntimePlan& target, std::size_t& cursor) {
    const auto* cpu = candidates[cursor++];
    target.assignments.push_back({role, cpu->id, cpu->numa_node});
  };
  add(ThreadRole::Network, plan, index);
  add(ThreadRole::Parser, plan, index);
  for (std::size_t i = 0; i < book_threads; ++i) add(ThreadRole::Book, plan, index);
  add(ThreadRole::Publisher, plan, index);
  add(ThreadRole::Control, plan, index);
  plan.valid = true;
  return plan;
}

RuntimePlan ValidateManualPlan(
    const HardwareTopology& topology,
    const std::vector<ManualAssignment>& assignments) noexcept {
  RuntimePlan plan;
  std::set<std::uint32_t> used;
  for (const auto& assignment : assignments) {
    const auto cpu = std::find_if(topology.cpus.begin(), topology.cpus.end(),
                                  [&](const CpuInfo& item) { return item.id == assignment.cpu && item.online; });
    if (cpu == topology.cpus.end()) {
      plan.warnings.emplace_back("manual plan references offline or unknown CPU");
      return plan;
    }
    if (!used.insert(assignment.cpu).second) {
      plan.warnings.emplace_back("manual plan assigns a CPU more than once");
      return plan;
    }
    plan.assignments.push_back({assignment.role, assignment.cpu, cpu->numa_node});
  }
  plan.valid = !assignments.empty();
  if (!plan.valid) plan.warnings.emplace_back("manual plan is empty");
  return plan;
}

int BindCpu(pthread_t thread, std::uint32_t cpu) noexcept {
  if (cpu >= CPU_SETSIZE) return EINVAL;
  cpu_set_t set;
  CPU_ZERO(&set);
  CPU_SET(cpu, &set);
  return pthread_setaffinity_np(thread, sizeof(set), &set);
}

int BindCurrentThread(std::uint32_t cpu) noexcept { return BindCpu(pthread_self(), cpu); }

}  // namespace utils::runtime

#include "mds/publish/wire_publisher.h"
#include "mds/transport/shared_ring.h"

#include <chrono>
#include <csignal>
#include <cstdint>
#include <cstdlib>
#include <cstring>
#include <fcntl.h>
#include <fstream>
#include <stdexcept>
#include <string>
#include <thread>
#include <unistd.h>
#include <utility>
#include <cstdio>
#include <sys/wait.h>
#include <vector>

namespace {

using namespace std::chrono_literals;

void require(bool condition, const char *message) {
  if (!condition) {
    throw std::runtime_error(message);
  }
}

bool wait_pid(pid_t pid, int options, int &status) {
  const auto waited = ::waitpid(pid, &status, options);
  return waited == pid;
}

bool still_running(pid_t pid) {
  int status = 0;
  const auto waited = ::waitpid(pid, &status, WNOHANG);
  if (waited == 0) {
    return ::kill(pid, 0) == 0;
  }
  return false;
}

std::uint64_t sigign_mask(pid_t pid) {
  std::ifstream input("/proc/" + std::to_string(pid) + "/status");
  std::string line;
  while (std::getline(input, line)) {
    if (!line.starts_with("SigIgn:")) {
      continue;
    }
    std::size_t offset = 7;
    while (offset < line.size() &&
           (line[offset] == ' ' || line[offset] == '\t')) {
      ++offset;
    }
    return std::stoull(line.substr(offset), nullptr, 16);
  }
  return 0;
}

bool ignores_sigpipe(pid_t pid) {
  return (sigign_mask(pid) & (1ULL << (SIGPIPE - 1))) != 0;
}

pid_t spawn(const std::vector<std::string> &args) {
  const auto pid = ::fork();
  require(pid >= 0, "fork failed");
  if (pid == 0) {
    const int devnull = ::open("/dev/null", O_RDWR);
    if (devnull >= 0) {
      ::dup2(devnull, STDOUT_FILENO);
      ::dup2(devnull, STDERR_FILENO);
      if (devnull > 2) {
        ::close(devnull);
      }
    }
    std::vector<char *> argv;
    argv.reserve(args.size() + 1);
    for (const auto &argument : args) {
      argv.push_back(const_cast<char *>(argument.c_str()));
    }
    argv.push_back(nullptr);
    ::execv(args.front().c_str(), argv.data());
    ::_exit(127);
  }
  return pid;
}

void terminate(pid_t pid) {
  if (pid <= 0) {
    return;
  }
  ::kill(pid, SIGTERM);
  for (int attempt = 0; attempt < 50; ++attempt) {
    int status = 0;
    if (wait_pid(pid, WNOHANG, status)) {
      return;
    }
    std::this_thread::sleep_for(10ms);
  }
  ::kill(pid, SIGKILL);
  int status = 0;
  (void)::waitpid(pid, &status, 0);
}

bool wait_for_reader(mds::transport::SharedRing &ring, pid_t pid) {
  for (int attempt = 0; attempt < 200; ++attempt) {
    if (ring.active_reader_count() >= 1U) {
      return true;
    }
    if (!still_running(pid)) {
      return false;
    }
    std::this_thread::sleep_for(10ms);
  }
  return false;
}

void test_clickhouse_mode_ignores_sigpipe() {
  const auto prefix =
      "/mds.sigpipe." + std::to_string(::getpid());
  const auto segment = mds::publish::make_multiplex_segment_name(
      prefix, "binance", "perpetual", "ticker", 0);
  require(!segment.empty(), "failed to build multiplex segment name");

  mds::transport::RingOptions options;
  options.name = segment;
  options.ring_bytes = 4096;
  options.max_record_bytes = 256;
  options.unlink_on_close = true;
  auto opened = mds::transport::SharedRing::open(options);
  require(bool(opened), opened.message.c_str());
  auto ring = std::move(opened.value);

  const auto yaml_path =
      "/tmp/mds.sigpipe." + std::to_string(::getpid()) + ".yaml";
  {
    std::ofstream yaml(yaml_path);
    require(static_cast<bool>(yaml), "failed to write ClickHouse consumer yaml");
    yaml << "clickhouse_bbo:\n"
         << "  host: 127.0.0.1\n"
         << "  service: \"1\"\n"
         << "  shm_prefix: " << prefix << "\n"
         << "  sample_interval_ms: 1000\n"
         << "  selectors:\n"
         << "    - {venue: binance, product: perpetual, shard: 0}\n";
  }

  const auto pid = spawn({MDS_SHM_CONSUMER_BIN, "--config", yaml_path,
                           "--clickhouse-bbo"});
  require(wait_for_reader(ring, pid),
          "clickhouse consumer did not register a reader");
  bool ignored = false;
  for (int attempt = 0; attempt < 200; ++attempt) {
    if (ignores_sigpipe(pid)) {
      ignored = true;
      break;
    }
    require(still_running(pid),
            "clickhouse consumer exited before ignoring SIGPIPE");
    std::this_thread::sleep_for(10ms);
  }
  require(ignored, "clickhouse consumer did not ignore SIGPIPE");
  require(::kill(pid, SIGPIPE) == 0, "failed to send SIGPIPE");
  std::this_thread::sleep_for(100ms);
  require(still_running(pid), "clickhouse consumer exited after SIGPIPE");
  terminate(pid);
  ::unlink(yaml_path.c_str());
}

void test_plain_mode_keeps_default_sigpipe() {
  const auto name =
      "/mds.sigpipe.plain." + std::to_string(::getpid());
  mds::transport::RingOptions options;
  options.name = name;
  options.ring_bytes = 4096;
  options.max_record_bytes = 256;
  options.unlink_on_close = true;
  auto opened = mds::transport::SharedRing::open(options);
  require(bool(opened), opened.message.c_str());
  auto ring = std::move(opened.value);

  const auto pid = spawn({MDS_SHM_CONSUMER_BIN, name});
  require(wait_for_reader(ring, pid),
          "plain consumer did not register a reader");
  require(!ignores_sigpipe(pid), "plain consumer ignored SIGPIPE");
  require(::kill(pid, SIGPIPE) == 0, "failed to send SIGPIPE to plain consumer");
  int status = 0;
  bool exited = false;
  for (int attempt = 0; attempt < 50; ++attempt) {
    if (wait_pid(pid, WNOHANG, status)) {
      exited = true;
      break;
    }
    std::this_thread::sleep_for(10ms);
  }
  if (!exited) {
    terminate(pid);
    throw std::runtime_error("plain consumer survived SIGPIPE");
  }
  require(WIFSIGNALED(status) && WTERMSIG(status) == SIGPIPE,
          "plain consumer was not terminated by SIGPIPE");
}

}  // namespace

int main() {
  try {
    test_clickhouse_mode_ignores_sigpipe();
    test_plain_mode_keeps_default_sigpipe();
    return 0;
  } catch (const std::exception &exception) {
    std::fprintf(stderr, "test failure: %s\n", exception.what());
    return 1;
  }
}

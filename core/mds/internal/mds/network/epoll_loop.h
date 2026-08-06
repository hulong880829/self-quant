#pragma once

#include <atomic>
#include <cstdint>
#include <functional>
#include <sys/epoll.h>
#include <unordered_map>
#include <vector>

namespace mds::network {

class EpollLoop {
public:
  using Callback = std::function<void(std::uint32_t)>;

  explicit EpollLoop(int max_events = 128);
  ~EpollLoop();
  EpollLoop(const EpollLoop &) = delete;
  EpollLoop &operator=(const EpollLoop &) = delete;

  bool add(int fd, std::uint32_t events, Callback callback);
  bool modify(int fd, std::uint32_t events);
  bool remove(int fd);
  int run_once(int timeout_ms);
  void run();
  void stop() noexcept;
  [[nodiscard]] int native_handle() const noexcept { return epoll_fd_; }

  static bool set_nonblocking(int fd) noexcept;

private:
  int epoll_fd_{-1};
  int wake_fd_{-1};
  int max_events_{128};
  std::atomic<bool> running_{false};
  std::unordered_map<int, Callback> callbacks_{};
  std::vector<epoll_event> events_{};
};

} // namespace mds::network

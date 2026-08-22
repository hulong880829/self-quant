#include "net/epoll_loop.h"

#include <cerrno>
#include <fcntl.h>
#include <sys/epoll.h>
#include <sys/eventfd.h>
#include <unistd.h>

namespace net {

EpollLoop::EpollLoop(int max_events)
    : max_events_(max_events > 0 ? max_events : 1),
      events_(static_cast<std::size_t>(max_events_)) {
  epoll_fd_ = ::epoll_create1(EPOLL_CLOEXEC);
  wake_fd_ = ::eventfd(0, EFD_NONBLOCK | EFD_CLOEXEC);
  if (epoll_fd_ >= 0 && wake_fd_ >= 0) {
    epoll_event event{};
    event.events = EPOLLIN;
    event.data.fd = wake_fd_;
    ::epoll_ctl(epoll_fd_, EPOLL_CTL_ADD, wake_fd_, &event);
  }
}

EpollLoop::~EpollLoop() {
  if (wake_fd_ >= 0) {
    ::close(wake_fd_);
  }
  if (epoll_fd_ >= 0) {
    ::close(epoll_fd_);
  }
}

bool EpollLoop::add(int fd, std::uint32_t events, Callback callback) {
  if (epoll_fd_ < 0 || fd < 0 || !set_nonblocking(fd)) {
    return false;
  }
  epoll_event event{};
  event.events = events;
  event.data.fd = fd;
  if (::epoll_ctl(epoll_fd_, EPOLL_CTL_ADD, fd, &event) != 0) {
    return false;
  }
  callbacks_.insert_or_assign(fd, std::move(callback));
  return true;
}

bool EpollLoop::modify(int fd, std::uint32_t events) {
  epoll_event event{};
  event.events = events;
  event.data.fd = fd;
  return ::epoll_ctl(epoll_fd_, EPOLL_CTL_MOD, fd, &event) == 0;
}

bool EpollLoop::remove(int fd) {
  callbacks_.erase(fd);
  return ::epoll_ctl(epoll_fd_, EPOLL_CTL_DEL, fd, nullptr) == 0 ||
         errno == ENOENT;
}

int EpollLoop::run_once(int timeout_ms) {
  if (epoll_fd_ < 0) {
    errno = EBADF;
    return -1;
  }
  const int count =
      ::epoll_wait(epoll_fd_, events_.data(), max_events_, timeout_ms);
  for (int i = 0; i < count; ++i) {
    const auto index = static_cast<std::size_t>(i);
    if (events_[index].data.fd == wake_fd_) {
      std::uint64_t value{};
      [[maybe_unused]] const auto bytes_read =
          ::read(wake_fd_, &value, sizeof(value));
      continue;
    }
    if (auto found = callbacks_.find(events_[index].data.fd);
        found != callbacks_.end()) {
      found->second(events_[index].events);
    }
  }
  return count;
}

void EpollLoop::run() {
  running_.store(true, std::memory_order_release);
  while (running_.load(std::memory_order_acquire)) {
    (void)run_once(-1);
  }
}

void EpollLoop::stop() noexcept {
  running_.store(false, std::memory_order_release);
  const std::uint64_t one = 1;
  if (wake_fd_ >= 0) {
    [[maybe_unused]] const auto bytes_written =
        ::write(wake_fd_, &one, sizeof(one));
  }
}

bool EpollLoop::set_nonblocking(int fd) noexcept {
  const int flags = ::fcntl(fd, F_GETFL, 0);
  return flags >= 0 && ::fcntl(fd, F_SETFL, flags | O_NONBLOCK) == 0;
}

} // namespace net

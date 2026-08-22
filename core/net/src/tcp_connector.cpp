#include "net/tcp_connector.h"

#include "net/epoll_loop.h"

#include <cerrno>
#include <cstring>
#include <netdb.h>
#include <netinet/tcp.h>
#include <sys/epoll.h>
#include <sys/socket.h>
#include <unistd.h>

namespace net {

TcpConnector::~TcpConnector() { reset(); }

void TcpConnector::close_socket() noexcept {
  if (fd_ >= 0) {
    ::close(fd_);
    fd_ = -1;
    ++socket_generation_;
  }
}

void TcpConnector::fail(int error, std::string_view message) noexcept {
  close_socket();
  last_error_ = error;
  error_message_ = message;
  state_ = ConnectState::Failed;
}

void TcpConnector::reset() noexcept {
  close_socket();
  if (addresses_) {
    if (resolver_hooks_) {
      resolver_hooks_->free_addresses(addresses_);
    } else {
      ::freeaddrinfo(addresses_);
    }
  }
  addresses_ = nullptr;
  next_address_ = nullptr;
  last_error_ = 0;
  state_ = ConnectState::Idle;
  deadline_ = {};
  error_message_ = {};
}

bool TcpConnector::start(std::string_view host, std::string_view service,
                         Clock::time_point deadline) noexcept {
  reset();
  state_ = ConnectState::Resolving;
  deadline_ = deadline;
  if (host.empty() || service.empty() || host.size() >= NI_MAXHOST ||
      service.size() >= NI_MAXSERV) {
    fail(EINVAL, "invalid TCP endpoint");
    return false;
  }
  if (Clock::now() >= deadline_) {
    last_error_ = ETIMEDOUT;
    error_message_ = "TCP connection timed out";
    state_ = ConnectState::TimedOut;
    return false;
  }

  char host_buffer[NI_MAXHOST]{};
  char service_buffer[NI_MAXSERV]{};
  std::memcpy(host_buffer, host.data(), host.size());
  std::memcpy(service_buffer, service.data(), service.size());
  addrinfo hints{};
  hints.ai_family = AF_UNSPEC;
  hints.ai_socktype = SOCK_STREAM;
  hints.ai_protocol = IPPROTO_TCP;
  const int result = resolver_hooks_
                         ? resolver_hooks_->resolve(host_buffer, service_buffer,
                                                    &hints, &addresses_)
                         : ::getaddrinfo(host_buffer, service_buffer, &hints,
                                         &addresses_);
  if (result != 0) {
    fail(result == EAI_SYSTEM ? errno : EHOSTUNREACH,
         "TCP name resolution failed");
    return false;
  }
  next_address_ = addresses_;
  return try_next_address();
}

bool TcpConnector::try_next_address() noexcept {
  while (next_address_) {
    const addrinfo *address = next_address_;
    next_address_ = next_address_->ai_next;
    close_socket();
    fd_ = ::socket(address->ai_family,
                   address->ai_socktype | SOCK_NONBLOCK | SOCK_CLOEXEC,
                   address->ai_protocol);
    if (fd_ < 0) {
      last_error_ = errno;
      continue;
    }
    ++socket_generation_;
    if (!EpollLoop::set_nonblocking(fd_)) {
      last_error_ = errno;
      continue;
    }
    const int nodelay = options_.tcp_nodelay ? 1 : 0;
    (void)::setsockopt(fd_, IPPROTO_TCP, TCP_NODELAY, &nodelay,
                       sizeof(nodelay));
    if (options_.receive_buffer_bytes > 0)
      (void)::setsockopt(fd_, SOL_SOCKET, SO_RCVBUF,
                         &options_.receive_buffer_bytes,
                         sizeof(options_.receive_buffer_bytes));
    if (options_.send_buffer_bytes > 0)
      (void)::setsockopt(fd_, SOL_SOCKET, SO_SNDBUF,
                         &options_.send_buffer_bytes,
                         sizeof(options_.send_buffer_bytes));
#ifdef SO_BUSY_POLL
    if (options_.busy_poll_us > 0)
      (void)::setsockopt(fd_, SOL_SOCKET, SO_BUSY_POLL,
                         &options_.busy_poll_us,
                         sizeof(options_.busy_poll_us));
#endif
    if (::connect(fd_, address->ai_addr, address->ai_addrlen) == 0) {
      state_ = ConnectState::Connected;
      error_message_ = {};
      return true;
    }
    if (errno == EINPROGRESS || errno == EWOULDBLOCK) {
      state_ = ConnectState::Connecting;
      error_message_ = {};
      return true;
    }
    last_error_ = errno;
  }
  fail(last_error_ != 0 ? last_error_ : ECONNREFUSED,
       "all TCP addresses failed");
  return false;
}

ConnectState TcpConnector::check_timeout(Clock::time_point now) noexcept {
  if ((state_ == ConnectState::Resolving ||
       state_ == ConnectState::Connecting) &&
      now >= deadline_) {
    close_socket();
    last_error_ = ETIMEDOUT;
    error_message_ = "TCP connection timed out";
    state_ = ConnectState::TimedOut;
  }
  return state_;
}

ConnectState TcpConnector::on_event(std::uint32_t events,
                                    Clock::time_point now) noexcept {
  if (check_timeout(now) != ConnectState::Connecting) {
    return state_;
  }
  if ((events & (EPOLLOUT | EPOLLERR | EPOLLHUP)) == 0) {
    return state_;
  }
  int socket_error = 0;
  socklen_t length = sizeof(socket_error);
  if (::getsockopt(fd_, SOL_SOCKET, SO_ERROR, &socket_error, &length) != 0) {
    socket_error = errno;
  }
  if (socket_error == 0) {
    state_ = ConnectState::Connected;
    last_error_ = 0;
    error_message_ = {};
    return state_;
  }
  last_error_ = socket_error;
  if (try_next_address()) {
    return state_;
  }
  return state_;
}

std::uint32_t TcpConnector::wanted_events() const noexcept {
  return state_ == ConnectState::Connecting
             ? static_cast<std::uint32_t>(EPOLLOUT | EPOLLERR | EPOLLHUP)
             : 0U;
}

} // namespace net

#include "net/tcp_acceptor.h"

#include <cerrno>
#include <cstdio>
#include <cstring>
#include <netdb.h>
#include <netinet/in.h>
#include <sys/socket.h>
#include <unistd.h>

namespace net {

TcpAcceptor::~TcpAcceptor() { reset(); }

void TcpAcceptor::reset() noexcept {
  if (fd_ >= 0) {
    ::close(fd_);
  }
  fd_ = -1;
  port_ = 0;
  last_error_ = 0;
  error_message_ = {};
}

void TcpAcceptor::fail(int error, std::string_view message) noexcept {
  if (fd_ >= 0) {
    ::close(fd_);
    fd_ = -1;
  }
  port_ = 0;
  last_error_ = error;
  error_message_ = message;
}

bool TcpAcceptor::open(std::string_view bind_address, std::uint16_t port,
                       int backlog, bool reuse_port) noexcept {
  reset();
  if (bind_address.empty() || bind_address.size() >= NI_MAXHOST ||
      backlog <= 0) {
    fail(EINVAL, "invalid TCP listen endpoint");
    return false;
  }

  char host[NI_MAXHOST]{};
  char service[6]{};
  std::memcpy(host, bind_address.data(), bind_address.size());
  const int service_length =
      std::snprintf(service, sizeof(service), "%u",
                    static_cast<unsigned int>(port));
  if (service_length <= 0 ||
      static_cast<std::size_t>(service_length) >= sizeof(service)) {
    fail(EINVAL, "invalid TCP listen port");
    return false;
  }

  addrinfo hints{};
  hints.ai_family = AF_UNSPEC;
  hints.ai_socktype = SOCK_STREAM;
  hints.ai_protocol = IPPROTO_TCP;
  hints.ai_flags = AI_NUMERICHOST | AI_PASSIVE;
  addrinfo *addresses = nullptr;
  const int resolved = ::getaddrinfo(host, service, &hints, &addresses);
  if (resolved != 0) {
    fail(resolved == EAI_SYSTEM ? errno : EADDRNOTAVAIL,
         "TCP bind address resolution failed");
    return false;
  }

  int attempt_error = EADDRNOTAVAIL;
  for (const addrinfo *address = addresses; address;
       address = address->ai_next) {
    fd_ = ::socket(address->ai_family,
                   address->ai_socktype | SOCK_NONBLOCK | SOCK_CLOEXEC,
                   address->ai_protocol);
    if (fd_ < 0) {
      attempt_error = errno;
      continue;
    }
    const int one = 1;
    if (::setsockopt(fd_, SOL_SOCKET, SO_REUSEADDR, &one, sizeof(one)) != 0) {
      attempt_error = errno;
      ::close(fd_);
      fd_ = -1;
      continue;
    }
    if (reuse_port &&
        ::setsockopt(fd_, SOL_SOCKET, SO_REUSEPORT, &one, sizeof(one)) != 0) {
      attempt_error = errno;
      ::close(fd_);
      fd_ = -1;
      continue;
    }
    if (::bind(fd_, address->ai_addr, address->ai_addrlen) != 0 ||
        ::listen(fd_, backlog) != 0) {
      attempt_error = errno;
      ::close(fd_);
      fd_ = -1;
      continue;
    }
    break;
  }
  ::freeaddrinfo(addresses);

  if (fd_ < 0) {
    fail(attempt_error, "TCP bind or listen failed");
    return false;
  }

  sockaddr_storage bound{};
  socklen_t bound_length = sizeof(bound);
  if (::getsockname(fd_, reinterpret_cast<sockaddr *>(&bound),
                    &bound_length) != 0) {
    fail(errno, "TCP bound address query failed");
    return false;
  }
  if (bound.ss_family == AF_INET) {
    port_ = ntohs(reinterpret_cast<const sockaddr_in *>(&bound)->sin_port);
  } else if (bound.ss_family == AF_INET6) {
    port_ = ntohs(reinterpret_cast<const sockaddr_in6 *>(&bound)->sin6_port);
  } else {
    fail(EAFNOSUPPORT, "unsupported TCP bound address family");
    return false;
  }
  error_message_ = {};
  return true;
}

int TcpAcceptor::accept_one() noexcept {
  if (fd_ < 0) {
    errno = EBADF;
    last_error_ = errno;
    return -1;
  }
  const int accepted =
      ::accept4(fd_, nullptr, nullptr, SOCK_NONBLOCK | SOCK_CLOEXEC);
  if (accepted < 0) {
    last_error_ = errno;
  } else {
    last_error_ = 0;
  }
  return accepted;
}

} // namespace net

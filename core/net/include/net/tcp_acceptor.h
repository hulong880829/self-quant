#pragma once

#include <cstdint>
#include <string_view>

namespace net {

class TcpAcceptor {
public:
  TcpAcceptor() = default;
  ~TcpAcceptor();
  TcpAcceptor(const TcpAcceptor &) = delete;
  TcpAcceptor &operator=(const TcpAcceptor &) = delete;

  bool open(std::string_view bind_address, std::uint16_t port, int backlog = 128,
            bool reuse_port = false) noexcept;
  int accept_one() noexcept;
  void reset() noexcept;

  [[nodiscard]] int fd() const noexcept { return fd_; }
  [[nodiscard]] std::uint16_t port() const noexcept { return port_; }
  [[nodiscard]] int last_error() const noexcept { return last_error_; }
  [[nodiscard]] std::string_view error_message() const noexcept {
    return error_message_;
  }

private:
  void fail(int error, std::string_view message) noexcept;

  int fd_{-1};
  std::uint16_t port_{};
  int last_error_{};
  std::string_view error_message_{};
};

} // namespace net

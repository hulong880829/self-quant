#pragma once

#include <chrono>
#include <cstdint>
#include <string_view>

struct addrinfo;

namespace net {

enum class ConnectState : std::uint8_t {
  Idle,
  Resolving,
  Connecting,
  Connected,
  TimedOut,
  Failed
};

struct SocketOptions {
  bool tcp_nodelay{true};
  std::int32_t receive_buffer_bytes{};
  std::int32_t send_buffer_bytes{};
  std::int32_t busy_poll_us{};
};

class TcpConnector {
public:
  using Clock = std::chrono::steady_clock;
  struct ResolverHooks {
    int (*resolve)(const char *, const char *, const addrinfo *,
                   addrinfo **) noexcept;
    void (*free_addresses)(addrinfo *) noexcept;
  };

  explicit TcpConnector(const ResolverHooks *resolver_hooks = nullptr) noexcept
      : resolver_hooks_(resolver_hooks) {}
  ~TcpConnector();
  TcpConnector(const TcpConnector &) = delete;
  TcpConnector &operator=(const TcpConnector &) = delete;

  bool start(std::string_view host, std::string_view service,
             Clock::time_point deadline) noexcept;
  ConnectState on_event(std::uint32_t events,
                        Clock::time_point now = Clock::now()) noexcept;
  ConnectState check_timeout(Clock::time_point now = Clock::now()) noexcept;
  void reset() noexcept;
  void set_socket_options(SocketOptions options) noexcept {
    options_ = options;
  }

  [[nodiscard]] ConnectState state() const noexcept { return state_; }
  [[nodiscard]] int fd() const noexcept { return fd_; }
  [[nodiscard]] std::uint64_t socket_generation() const noexcept {
    return socket_generation_;
  }
  [[nodiscard]] int last_error() const noexcept { return last_error_; }
  [[nodiscard]] std::string_view error_message() const noexcept {
    return error_message_;
  }
  [[nodiscard]] std::uint32_t wanted_events() const noexcept;

private:
  bool try_next_address() noexcept;
  void close_socket() noexcept;
  void fail(int error, std::string_view message) noexcept;

  addrinfo *addresses_{nullptr};
  addrinfo *next_address_{nullptr};
  const ResolverHooks *resolver_hooks_{nullptr};
  int fd_{-1};
  int last_error_{};
  std::uint64_t socket_generation_{};
  ConnectState state_{ConnectState::Idle};
  Clock::time_point deadline_{};
  std::string_view error_message_{};
  SocketOptions options_{};
};

} // namespace net

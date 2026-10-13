#pragma once

#include <cstddef>
#include <cstdint>
#include <optional>
#include <span>
#include <string>
#include <string_view>
#include <utility>
#include <vector>

namespace net {

enum class HttpParseStatus : std::uint8_t {
  NeedMore,
  Complete,
  Error,
};

struct HttpLimits {
  std::size_t max_header_bytes{16U << 10U};
  std::size_t max_body_bytes{64U << 10U};
};

struct HttpRequest {
  std::string method;
  std::string target;
  std::string path;
  std::string query;
  std::string version;
  std::vector<std::pair<std::string, std::string>> headers;
  std::vector<std::byte> body;
  bool keep_alive{true};
  bool websocket_upgrade{};
};

[[nodiscard]] bool percent_decode(std::string_view input, std::string &output,
                                  bool plus_as_space) noexcept;
[[nodiscard]] std::string_view header_value(const HttpRequest &request,
                                            std::string_view name) noexcept;
[[nodiscard]] std::optional<std::string>
query_value(std::string_view query, std::string_view key) noexcept;

class HttpRequestParser {
 public:
  explicit HttpRequestParser(HttpLimits limits = {});

  HttpParseStatus feed(std::span<const std::byte> bytes, std::size_t &consumed,
                       std::string_view &error) noexcept;
  void reset() noexcept;
  [[nodiscard]] bool complete() const noexcept { return complete_; }
  [[nodiscard]] const HttpRequest &request() const noexcept { return request_; }
  [[nodiscard]] std::string_view error_message() const noexcept {
    return error_;
  }

 private:
  bool parse_available(std::string_view &error) noexcept;
  bool parse_request_line(std::string_view line,
                          std::string_view &error) noexcept;
  bool parse_header_line(std::string_view line,
                         std::string_view &error) noexcept;
  bool finish_headers(std::string_view &error) noexcept;

  HttpLimits limits_{};
  std::vector<std::byte> buffer_{};
  std::size_t parsed_{};
  std::size_t body_needed_{};
  bool headers_done_{};
  bool complete_{};
  HttpRequest request_{};
  std::string error_{};
};

class HttpResponseWriter {
 public:
  void reset() noexcept;
  void set_status(int status, std::string_view reason) noexcept;
  void set_header(std::string_view name, std::string_view value);
  void set_body(std::string_view content_type, std::string_view body);
  void set_body(std::string_view content_type,
                std::span<const std::byte> body);
  [[nodiscard]] std::string serialize(bool keep_alive) const;

 private:
  int status_{200};
  std::string reason_{"OK"};
  std::vector<std::pair<std::string, std::string>> headers_{};
  std::string content_type_{"text/plain; charset=utf-8"};
  std::string body_{};
};

}  // namespace net

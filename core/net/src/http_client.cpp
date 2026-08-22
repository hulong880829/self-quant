#include "net/http_client.h"

#include <algorithm>
#include <charconv>
#include <cstring>
#include <limits>
#include <sys/epoll.h>

namespace net {
namespace {

std::string_view trim(std::string_view value) noexcept {
  while (!value.empty() && (value.front() == ' ' || value.front() == '\t')) {
    value.remove_prefix(1);
  }
  while (!value.empty() && (value.back() == ' ' || value.back() == '\t')) {
    value.remove_suffix(1);
  }
  return value;
}

bool ascii_iequals(std::string_view left, std::string_view right) noexcept {
  if (left.size() != right.size()) {
    return false;
  }
  for (std::size_t i = 0; i < left.size(); ++i) {
    char a = left[i];
    char b = right[i];
    if (a >= 'A' && a <= 'Z') {
      a = static_cast<char>(a + ('a' - 'A'));
    }
    if (b >= 'A' && b <= 'Z') {
      b = static_cast<char>(b + ('a' - 'A'));
    }
    if (a != b) {
      return false;
    }
  }
  return true;
}

bool valid_header_name(std::string_view value) noexcept {
  if (value.empty()) {
    return false;
  }
  for (const char raw_character : value) {
    const auto character = static_cast<unsigned char>(raw_character);
    const bool token = (character >= 'a' && character <= 'z') ||
                       (character >= 'A' && character <= 'Z') ||
                       (character >= '0' && character <= '9') ||
                       std::string_view("!#$%&'*+-.^_`|~").find(
                           static_cast<char>(character)) !=
                           std::string_view::npos;
    if (!token) {
      return false;
    }
  }
  return true;
}

bool valid_header_value(std::string_view value) noexcept {
  for (const char raw_character : value) {
    const auto character = static_cast<unsigned char>(raw_character);
    if (character == '\r' || character == '\n' ||
        (character < 0x20U && character != '\t') || character == 0x7fU) {
      return false;
    }
  }
  return true;
}

bool valid_host(std::string_view host) noexcept {
  if (host.empty() || !valid_header_value(host)) {
    return false;
  }
  for (const char raw_character : host) {
    const auto character = static_cast<unsigned char>(raw_character);
    if (character <= 0x20U || character == 0x7fU || raw_character == '/' ||
        raw_character == '?' || raw_character == '#' || raw_character == '@') {
      return false;
    }
  }
  return true;
}

bool valid_target(std::string_view target) noexcept {
  if (target.empty() || target.front() != '/') {
    return false;
  }
  for (const char raw_character : target) {
    const auto character = static_cast<unsigned char>(raw_character);
    if (character <= 0x20U || character == 0x7fU) {
      return false;
    }
  }
  return true;
}

bool reserved_header(std::string_view name) noexcept {
  return ascii_iequals(name, "Host") ||
         ascii_iequals(name, "Content-Length") ||
         ascii_iequals(name, "Content-Type") ||
         ascii_iequals(name, "Connection") ||
         ascii_iequals(name, "User-Agent");
}

std::string_view method_name(HttpMethod method) noexcept {
  switch (method) {
  case HttpMethod::Get:
    return "GET";
  case HttpMethod::Post:
    return "POST";
  case HttpMethod::Put:
    return "PUT";
  case HttpMethod::Delete:
    return "DELETE";
  }
  return {};
}

bool valid_http_request(std::string_view host, const HttpRequest &request,
                        std::size_t max_custom_headers) noexcept {
  const std::string_view method = method_name(request.method);
  if (method.empty() || !valid_host(host) || !valid_target(request.target) ||
      !valid_header_value(request.content_type) ||
      request.headers.size() > max_custom_headers ||
      (!request.body.empty() && request.content_type.empty())) {
    return false;
  }
  for (const HttpHeader &header : request.headers) {
    if (!valid_header_name(header.name) || !valid_header_value(header.value) ||
        reserved_header(header.name)) {
      return false;
    }
  }
  return true;
}

} // namespace

HttpStatusClass classify_http_status(unsigned status) noexcept {
  if (status == 418 || status == 429) {
    return HttpStatusClass::RateLimited;
  }
  if (status >= 100 && status < 200) {
    return HttpStatusClass::Informational;
  }
  if (status >= 200 && status < 300) {
    return HttpStatusClass::Success;
  }
  if (status >= 300 && status < 400) {
    return HttpStatusClass::Redirect;
  }
  if (status >= 400 && status < 500) {
    return HttpStatusClass::ClientError;
  }
  if (status >= 500 && status < 600) {
    return HttpStatusClass::ServerError;
  }
  return HttpStatusClass::Invalid;
}

bool encode_http_request(std::string_view host, const HttpRequest &request,
                         std::span<std::byte> output,
                         std::size_t &output_size,
                         std::size_t max_custom_headers) noexcept {
  output_size = 0;
  const std::string_view method = method_name(request.method);
  if (!valid_http_request(host, request, max_custom_headers)) {
    return false;
  }

  const auto append = [&output, &output_size](std::string_view value) noexcept {
    if (value.size() > output.size() - output_size) {
      return false;
    }
    if (value.empty()) {
      return true;
    }
    std::memcpy(output.data() + output_size, value.data(), value.size());
    output_size += value.size();
    return true;
  };
  if (!append(method) || !append(" ") || !append(request.target) ||
      !append(" HTTP/1.1\r\nHost: ") || !append(host) ||
      !append("\r\nAccept: application/json\r\n")) {
    output_size = 0;
    return false;
  }
  for (const HttpHeader &header : request.headers) {
    if (!append(header.name) || !append(": ") || !append(header.value) ||
        !append("\r\n")) {
      output_size = 0;
      return false;
    }
  }
  if (!request.content_type.empty()) {
    if (!append("Content-Type: ") || !append(request.content_type) ||
        !append("\r\n")) {
      output_size = 0;
      return false;
    }
  }
  if (!request.body.empty() || request.method == HttpMethod::Post ||
      request.method == HttpMethod::Put) {
    char length[32]{};
    const auto converted =
        std::to_chars(length, length + sizeof(length), request.body.size());
    if (converted.ec != std::errc{} || !append("Content-Length: ") ||
        !append({length, static_cast<std::size_t>(converted.ptr - length)}) ||
        !append("\r\n")) {
      output_size = 0;
      return false;
    }
  }
  if (!append("Connection: close\r\n"
              "User-Agent: self-quant-mds/1\r\n\r\n") ||
      !append({reinterpret_cast<const char *>(request.body.data()),
               request.body.size()})) {
    output_size = 0;
    return false;
  }
  return true;
}

HttpResponseParser::HttpResponseParser(std::size_t header_capacity,
                                       std::size_t body_capacity)
    : headers_(header_capacity), body_(body_capacity) {}

std::string_view HttpResponseParser::header_value(
    std::string_view name) const noexcept {
  if (!headers_complete_ || name.empty()) return {};
  const std::string_view block(
      reinterpret_cast<const char*>(headers_.data()), header_used_);
  std::size_t begin = block.find("\r\n");
  if (begin == std::string_view::npos) return {};
  begin += 2;
  while (begin < block.size()) {
    const std::size_t end = block.find("\r\n", begin);
    if (end == std::string_view::npos || end == begin) break;
    const std::string_view line = block.substr(begin, end - begin);
    const std::size_t colon = line.find(':');
    if (colon != std::string_view::npos &&
        ascii_iequals(trim(line.substr(0, colon)), name)) {
      return trim(line.substr(colon + 1));
    }
    begin = end + 2;
  }
  return {};
}

void HttpResponseParser::reset() noexcept {
  header_used_ = 0;
  body_used_ = 0;
  content_remaining_ = 0;
  chunk_remaining_ = 0;
  chunk_line_used_ = 0;
  status_code_ = 0;
  body_mode_ = BodyMode::None;
  chunk_state_ = ChunkState::Size;
  parse_error_ = HttpParseError::None;
  headers_complete_ = false;
  complete_ = false;
}

bool HttpResponseParser::reject(HttpParseError code, std::string_view message,
                                std::string_view &error) noexcept {
  parse_error_ = code;
  error = message;
  return false;
}

bool HttpResponseParser::parse_headers(std::string_view &error) noexcept {
  const std::string_view headers{
      reinterpret_cast<const char *>(headers_.data()), header_used_};
  const auto status_end = headers.find("\r\n");
  if (status_end == std::string_view::npos) {
    error = "invalid HTTP status line";
    return false;
  }
  const auto status_line = headers.substr(0, status_end);
  if (status_line.size() < 12 ||
      !(status_line.starts_with("HTTP/1.0 ") ||
        status_line.starts_with("HTTP/1.1 ")) ||
      (status_line.size() > 12 && status_line[12] != ' ')) {
    error = "invalid HTTP status line";
    return false;
  }
  const auto code_text = status_line.substr(9, 3);
  const auto code_result = std::from_chars(code_text.data(),
                                           code_text.data() + code_text.size(),
                                           status_code_);
  if (code_result.ec != std::errc{} || code_result.ptr != code_text.data() + 3 ||
      status_code_ < 100 || status_code_ > 599) {
    error = "invalid HTTP status code";
    return false;
  }

  bool has_content_length = false;
  bool has_transfer_encoding = false;
  std::size_t position = status_end + 2;
  while (position < headers.size()) {
    const auto end = headers.find("\r\n", position);
    if (end == std::string_view::npos || end == position) {
      break;
    }
    const auto line = headers.substr(position, end - position);
    const auto colon = line.find(':');
    if (colon == std::string_view::npos) {
      error = "malformed HTTP header";
      return false;
    }
    const auto name = line.substr(0, colon);
    const auto value = trim(line.substr(colon + 1));
    if (!valid_header_name(name) || !valid_header_value(value)) {
      error = "malformed HTTP header";
      return false;
    }
    if (ascii_iequals(name, "Content-Length")) {
      if (has_content_length) {
        error = "duplicate HTTP Content-Length";
        return false;
      }
      const auto result = std::from_chars(
          value.data(), value.data() + value.size(), content_remaining_);
      if (result.ec != std::errc{} ||
          result.ptr != value.data() + value.size()) {
        error = "invalid HTTP Content-Length";
        return false;
      }
      has_content_length = true;
    } else if (ascii_iequals(name, "Transfer-Encoding")) {
      if (has_transfer_encoding || !ascii_iequals(value, "chunked")) {
        error = "unsupported HTTP Transfer-Encoding";
        return false;
      }
      has_transfer_encoding = true;
    }
    position = end + 2;
  }
  if (has_transfer_encoding && has_content_length) {
    error = "ambiguous HTTP body framing";
    return false;
  }
  headers_complete_ = true;
  const bool no_body = status_code_ == 101 || status_code_ == 204 ||
                       status_code_ == 304;
  if (no_body || (has_content_length && content_remaining_ == 0)) {
    body_mode_ = BodyMode::None;
    complete_ = true;
  } else if (status_code_ >= 100 && status_code_ < 200) {
    body_mode_ = BodyMode::None;
  } else if (has_transfer_encoding) {
    body_mode_ = BodyMode::Chunked;
  } else if (has_content_length) {
    if (content_remaining_ > body_.size()) {
      return reject(HttpParseError::BodyTooLarge,
                    "HTTP body exceeds configured capacity", error);
    }
    body_mode_ = BodyMode::ContentLength;
  } else {
    body_mode_ = BodyMode::UntilClose;
  }
  return true;
}

bool HttpResponseParser::parse_trailers(std::string_view &error) noexcept {
  const std::string_view trailers{
      reinterpret_cast<const char *>(headers_.data()), header_used_};
  if (trailers == "\r\n") {
    return true;
  }
  std::size_t position = 0;
  while (position < trailers.size()) {
    const auto end = trailers.find("\r\n", position);
    if (end == std::string_view::npos) {
      error = "malformed HTTP trailer";
      return false;
    }
    if (end == position) {
      return end + 2 == trailers.size();
    }
    const auto line = trailers.substr(position, end - position);
    const auto colon = line.find(':');
    if (colon == std::string_view::npos) {
      error = "malformed HTTP trailer";
      return false;
    }
    const auto name = line.substr(0, colon);
    const auto value = trim(line.substr(colon + 1));
    if (!valid_header_name(name) || !valid_header_value(value) ||
        ascii_iequals(name, "Content-Length") ||
        ascii_iequals(name, "Transfer-Encoding")) {
      error = "invalid HTTP trailer";
      return false;
    }
    position = end + 2;
  }
  error = "malformed HTTP trailer";
  return false;
}

bool HttpResponseParser::append_body(std::span<const std::byte> bytes,
                                     std::string_view &error) noexcept {
  if (bytes.size() > body_.size() - body_used_) {
    return reject(HttpParseError::BodyTooLarge,
                  "HTTP body exceeds configured capacity", error);
  }
  if (bytes.empty()) {
    return true;
  }
  std::memcpy(body_.data() + body_used_, bytes.data(), bytes.size());
  body_used_ += bytes.size();
  return true;
}

bool HttpResponseParser::feed_body(std::span<const std::byte> bytes,
                                   std::string_view &error) noexcept {
  if (body_mode_ == BodyMode::ContentLength) {
    const std::size_t take = std::min(bytes.size(), content_remaining_);
    if (!append_body(bytes.first(take), error)) {
      return false;
    }
    content_remaining_ -= take;
    complete_ = content_remaining_ == 0;
    if (take != bytes.size()) {
      error = "bytes received after HTTP response";
      return false;
    }
    return true;
  }
  if (body_mode_ == BodyMode::UntilClose) {
    return append_body(bytes, error);
  }
  if (body_mode_ != BodyMode::Chunked) {
    return bytes.empty();
  }

  std::size_t position = 0;
  while (position < bytes.size() && !complete_) {
    if (chunk_state_ == ChunkState::Size) {
      const char value = static_cast<char>(bytes[position++]);
      if (chunk_line_used_ >= sizeof(chunk_line_)) {
        error = "HTTP chunk size line too long";
        return false;
      }
      chunk_line_[chunk_line_used_++] = value;
      if (chunk_line_used_ >= 2 &&
          chunk_line_[chunk_line_used_ - 2] == '\r' &&
          chunk_line_[chunk_line_used_ - 1] == '\n') {
        std::string_view line(chunk_line_, chunk_line_used_ - 2);
        const auto extension = line.find(';');
        line = trim(line.substr(0, extension));
        if (line.empty()) {
          error = "empty HTTP chunk size";
          return false;
        }
        const auto result = std::from_chars(
            line.data(), line.data() + line.size(), chunk_remaining_, 16);
        if (result.ec != std::errc{} ||
            result.ptr != line.data() + line.size()) {
          error = "invalid HTTP chunk size";
          return false;
        }
        chunk_line_used_ = 0;
        if (chunk_remaining_ == 0) {
          chunk_state_ = ChunkState::Trailers;
          header_used_ = 0;
        } else {
          if (chunk_remaining_ > body_.size() - body_used_) {
            return reject(HttpParseError::BodyTooLarge,
                          "HTTP body exceeds configured capacity", error);
          }
          chunk_state_ = ChunkState::Data;
        }
      }
    } else if (chunk_state_ == ChunkState::Data) {
      const std::size_t take =
          std::min(chunk_remaining_, bytes.size() - position);
      if (!append_body(bytes.subspan(position, take), error)) {
        return false;
      }
      position += take;
      chunk_remaining_ -= take;
      if (chunk_remaining_ == 0) {
        chunk_state_ = ChunkState::DataCrlf;
        chunk_line_used_ = 0;
      }
    } else if (chunk_state_ == ChunkState::DataCrlf) {
      const char expected[2] = {'\r', '\n'};
      const char value = static_cast<char>(bytes[position++]);
      if (value != expected[chunk_line_used_]) {
        error = "missing CRLF after HTTP chunk";
        return false;
      }
      if (++chunk_line_used_ == 2) {
        chunk_line_used_ = 0;
        chunk_state_ = ChunkState::Size;
      }
    } else {
      if (header_used_ == headers_.size()) {
        return reject(HttpParseError::HeaderTooLarge,
                      "HTTP trailers exceed configured capacity", error);
      }
      headers_[header_used_++] = bytes[position++];
      if ((header_used_ == 2 && headers_[0] == std::byte{'\r'} &&
           headers_[1] == std::byte{'\n'}) ||
          (header_used_ >= 4 &&
           headers_[header_used_ - 4] == std::byte{'\r'} &&
           headers_[header_used_ - 3] == std::byte{'\n'} &&
           headers_[header_used_ - 2] == std::byte{'\r'} &&
           headers_[header_used_ - 1] == std::byte{'\n'})) {
        if (!parse_trailers(error)) {
          return false;
        }
        complete_ = true;
      }
    }
  }
  if (complete_ && position != bytes.size()) {
    error = "bytes received after HTTP response";
    return false;
  }
  return true;
}

bool HttpResponseParser::feed(std::span<const std::byte> bytes,
                              std::string_view &error) noexcept {
  if (parse_error_ != HttpParseError::None) {
    error = "HTTP parser is in failed state";
    return false;
  }
  if (complete_) {
    if (!bytes.empty()) {
      error = "bytes received after HTTP response";
      return false;
    }
    return true;
  }
  if (headers_complete_) {
    const bool accepted = feed_body(bytes, error);
    if (!accepted && parse_error_ == HttpParseError::None) {
      parse_error_ = HttpParseError::InvalidResponse;
    }
    return accepted;
  }
  std::size_t position = 0;
  while (position < bytes.size()) {
    if (header_used_ == headers_.size()) {
      return reject(HttpParseError::HeaderTooLarge,
                    "HTTP headers exceed configured capacity", error);
    }
    headers_[header_used_++] = bytes[position++];
    if (header_used_ >= 4 && headers_[header_used_ - 4] == std::byte{'\r'} &&
        headers_[header_used_ - 3] == std::byte{'\n'} &&
        headers_[header_used_ - 2] == std::byte{'\r'} &&
        headers_[header_used_ - 1] == std::byte{'\n'}) {
      if (!parse_headers(error)) {
        if (parse_error_ == HttpParseError::None) {
          parse_error_ = HttpParseError::InvalidResponse;
        }
        return false;
      }
      if (status_code_ >= 100 && status_code_ < 200 && status_code_ != 101) {
        header_used_ = 0;
        content_remaining_ = 0;
        status_code_ = 0;
        body_mode_ = BodyMode::None;
        headers_complete_ = false;
        if (position == bytes.size()) {
          return true;
        }
        continue;
      }
      const bool accepted =
          complete_ ? position == bytes.size()
                    : feed_body(bytes.subspan(position), error);
      if (!accepted && parse_error_ == HttpParseError::None) {
        parse_error_ = HttpParseError::InvalidResponse;
      }
      return accepted;
    }
  }
  return true;
}

bool HttpResponseParser::finish(std::string_view &error) noexcept {
  if (parse_error_ != HttpParseError::None) {
    error = "HTTP parser is in failed state";
    return false;
  }
  if (complete_) {
    return true;
  }
  if (headers_complete_ && body_mode_ == BodyMode::UntilClose) {
    complete_ = true;
    return true;
  }
  return reject(HttpParseError::InvalidResponse,
                "HTTP connection closed before response completed", error);
}

HttpClient::HttpClient(SharedSslContext context, std::size_t header_capacity,
                       std::size_t body_capacity,
                       std::size_t request_capacity,
                       std::size_t host_capacity,
                       std::size_t tls_receive_capacity)
    : context_(std::move(context)), parser_(header_capacity, body_capacity),
      request_(request_capacity), tls_receive_(tls_receive_capacity),
      host_(host_capacity + 1) {}

void HttpClient::reset() noexcept {
  tls_.reset();
  connector_.reset();
  parser_.reset();
  std::fill(request_.begin(), request_.end(), std::byte{});
  request_size_ = 0;
  request_offset_ = 0;
  state_ = HttpClientState::Idle;
  wanted_events_ = 0;
  error_code_ = HttpClientError::None;
  error_ = {};
}

void HttpClient::fail(HttpClientError code, std::string_view error) noexcept {
  state_ = HttpClientState::Failed;
  wanted_events_ = 0;
  error_code_ = code;
  error_ = error;
}

bool HttpClient::start_get(std::string_view host, std::string_view service,
                           std::string_view target,
                           Clock::time_point deadline) noexcept {
  return start_request(host, service, {HttpMethod::Get, target, {}, {}, {}},
                       deadline);
}

bool HttpClient::start_post(std::string_view host, std::string_view service,
                            std::string_view target,
                            std::string_view content_type,
                            std::span<const std::byte> body,
                            Clock::time_point deadline) noexcept {
  return start_request(
      host, service, {HttpMethod::Post, target, content_type, body, {}},
      deadline);
}

bool HttpClient::start_put(std::string_view host, std::string_view service,
                           std::string_view target,
                           std::string_view content_type,
                           std::span<const std::byte> body,
                           Clock::time_point deadline) noexcept {
  return start_request(
      host, service, {HttpMethod::Put, target, content_type, body, {}},
      deadline);
}

bool HttpClient::start_delete(std::string_view host, std::string_view service,
                              std::string_view target,
                              Clock::time_point deadline) noexcept {
  return start_request(host, service,
                       {HttpMethod::Delete, target, {}, {}, {}}, deadline);
}

bool HttpClient::start_request(std::string_view host, std::string_view service,
                               const HttpRequest &request,
                               Clock::time_point deadline) noexcept {
  reset();
  if (!context_ || service.empty() || host.size() + 1 > host_.size()) {
    fail(HttpClientError::InvalidRequest, "invalid HTTPS request parameters");
    return false;
  }
  std::memcpy(host_.data(), host.data(), host.size());
  host_[host.size()] = '\0';
  if (!valid_http_request(host, request, 32)) {
    fail(HttpClientError::InvalidRequest, "invalid HTTPS request parameters");
    return false;
  }
  if (!encode_http_request(host, request, request_, request_size_)) {
    fail(HttpClientError::RequestTooLarge,
         "HTTPS request exceeds configured capacity");
    return false;
  }
  deadline_ = deadline;
  if (!connector_.start(host, service, deadline)) {
    if (connector_.state() == ConnectState::TimedOut) {
      state_ = HttpClientState::TimedOut;
      error_code_ = HttpClientError::Timeout;
      error_ = "HTTPS request timed out";
      return false;
    }
    fail(HttpClientError::Connect, connector_.error_message());
    return false;
  }
  if (connector_.state() == ConnectState::Connected) {
    return begin_tls();
  }
  state_ = HttpClientState::TcpConnecting;
  wanted_events_ = connector_.wanted_events();
  return true;
}

bool HttpClient::begin_tls() noexcept {
  tls_.emplace(context_, connector_.fd(), std::string_view(host_.data()),
               std::span<std::byte>(tls_receive_));
  if (!tls_->native_handle()) {
    fail(HttpClientError::Tls, "failed to create TLS session");
    return false;
  }
  state_ = HttpClientState::TlsHandshaking;
  drive_tls();
  return state_ != HttpClientState::Failed;
}

void HttpClient::drive_tls() noexcept {
  const int result = tls_->handshake();
  if (result == 1) {
    state_ = HttpClientState::SendingRequest;
    wanted_events_ = EPOLLOUT | EPOLLERR | EPOLLHUP;
    drive_write();
  } else {
    wanted_events_ = static_cast<std::uint32_t>(tls_->wanted_events(result));
    if (wanted_events_ == EPOLLERR) {
      fail(HttpClientError::Tls, "TLS handshake failed");
    }
  }
}

void HttpClient::drive_write() noexcept {
  while (request_offset_ < request_size_) {
    const int result = tls_->write(
        {request_.data() + request_offset_, request_size_ - request_offset_});
    if (result > 0) {
      request_offset_ += static_cast<std::size_t>(result);
      continue;
    }
    wanted_events_ = static_cast<std::uint32_t>(tls_->wanted_events(result));
    if (wanted_events_ == EPOLLERR) {
      fail(HttpClientError::Transport, "HTTPS request write failed");
    }
    return;
  }
  state_ = HttpClientState::ReadingResponse;
  wanted_events_ = EPOLLIN | EPOLLERR | EPOLLHUP;
}

void HttpClient::drive_read() noexcept {
  for (;;) {
    std::span<const std::byte> bytes;
    const int result = tls_->read_some(bytes);
    if (result > 0) {
      if (!parser_.feed(bytes, error_)) {
        const HttpParseError parse_error = parser_.error_code();
        fail(parse_error == HttpParseError::HeaderTooLarge ||
                     parse_error == HttpParseError::BodyTooLarge
                 ? HttpClientError::ResponseTooLarge
                 : HttpClientError::InvalidResponse,
             error_);
        return;
      }
      if (parser_.complete()) {
        state_ = HttpClientState::Complete;
        wanted_events_ = 0;
        return;
      }
      continue;
    }
    if (result == 0) {
      if (parser_.finish(error_)) {
        state_ = HttpClientState::Complete;
        wanted_events_ = 0;
      } else {
        fail(HttpClientError::InvalidResponse, error_);
      }
      return;
    }
    wanted_events_ = static_cast<std::uint32_t>(tls_->wanted_events(result));
    if (wanted_events_ == EPOLLERR) {
      fail(HttpClientError::Transport, "HTTPS response read failed");
    }
    return;
  }
}

HttpClientState HttpClient::check_timeout(Clock::time_point now) noexcept {
  if (state_ != HttpClientState::Idle &&
      state_ != HttpClientState::Complete &&
      state_ != HttpClientState::Failed && now >= deadline_) {
    state_ = HttpClientState::TimedOut;
    wanted_events_ = 0;
    error_code_ = HttpClientError::Timeout;
    error_ = "HTTPS request timed out";
  }
  return state_;
}

HttpClientState HttpClient::on_event(std::uint32_t events,
                                     Clock::time_point now) noexcept {
  if (check_timeout(now) == HttpClientState::TimedOut) {
    return state_;
  }
  if (state_ == HttpClientState::TcpConnecting) {
    const ConnectState connected = connector_.on_event(events, now);
    if (connected == ConnectState::Connected) {
      begin_tls();
    } else if (connected == ConnectState::Failed ||
               connected == ConnectState::TimedOut) {
      if (connected == ConnectState::TimedOut) {
        state_ = HttpClientState::TimedOut;
        wanted_events_ = 0;
        error_code_ = HttpClientError::Timeout;
        error_ = "HTTPS request timed out";
      } else {
        fail(HttpClientError::Connect, connector_.error_message());
      }
    } else {
      wanted_events_ = connector_.wanted_events();
    }
  } else if (state_ == HttpClientState::TlsHandshaking) {
    drive_tls();
  } else if (state_ == HttpClientState::SendingRequest) {
    drive_write();
  } else if (state_ == HttpClientState::ReadingResponse) {
    drive_read();
  }
  return state_;
}

std::uint32_t HttpClient::wanted_events() const noexcept {
  return wanted_events_;
}

} // namespace net

#include "mds/network/http_client.h"

#include <algorithm>
#include <charconv>
#include <cstring>
#include <limits>
#include <sys/epoll.h>

namespace mds::network {
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

bool contains_token(std::string_view value, std::string_view wanted) noexcept {
  std::size_t position = 0;
  while (position < value.size()) {
    const auto comma = value.find(',', position);
    const auto token = trim(value.substr(
        position, comma == std::string_view::npos ? value.size() - position
                                                  : comma - position));
    if (ascii_iequals(token, wanted)) {
      return true;
    }
    if (comma == std::string_view::npos) {
      break;
    }
    position = comma + 1;
  }
  return false;
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

HttpResponseParser::HttpResponseParser(std::size_t header_capacity,
                                       std::size_t body_capacity)
    : headers_(header_capacity), body_(body_capacity) {}

void HttpResponseParser::reset() noexcept {
  header_used_ = 0;
  body_used_ = 0;
  content_remaining_ = 0;
  chunk_remaining_ = 0;
  chunk_line_used_ = 0;
  trailer_match_ = 0;
  status_code_ = 0;
  body_mode_ = BodyMode::None;
  chunk_state_ = ChunkState::Size;
  headers_complete_ = false;
  complete_ = false;
}

bool HttpResponseParser::parse_headers(std::string_view &error) noexcept {
  const std::string_view headers{
      reinterpret_cast<const char *>(headers_.data()), header_used_};
  const auto status_end = headers.find("\r\n");
  if (status_end == std::string_view::npos ||
      !headers.substr(0, status_end).starts_with("HTTP/1.")) {
    error = "invalid HTTP status line";
    return false;
  }
  const auto status_line = headers.substr(0, status_end);
  const auto space = status_line.find(' ');
  if (space == std::string_view::npos) {
    error = "missing HTTP status code";
    return false;
  }
  const auto code_text = status_line.substr(space + 1, 3);
  const auto code_result = std::from_chars(code_text.data(),
                                           code_text.data() + code_text.size(),
                                           status_code_);
  if (code_result.ec != std::errc{} || code_result.ptr != code_text.data() + 3 ||
      status_code_ < 100 || status_code_ > 599) {
    error = "invalid HTTP status code";
    return false;
  }

  bool has_content_length = false;
  bool chunked = false;
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
    const auto name = trim(line.substr(0, colon));
    const auto value = trim(line.substr(colon + 1));
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
      chunked = contains_token(value, "chunked");
      if (!chunked) {
        error = "unsupported HTTP Transfer-Encoding";
        return false;
      }
    }
    position = end + 2;
  }
  if (chunked && has_content_length) {
    error = "ambiguous HTTP body framing";
    return false;
  }
  headers_complete_ = true;
  const bool no_body = (status_code_ >= 100 && status_code_ < 200) ||
                       status_code_ == 204 || status_code_ == 304;
  if (no_body || (has_content_length && content_remaining_ == 0)) {
    body_mode_ = BodyMode::None;
    complete_ = true;
  } else if (chunked) {
    body_mode_ = BodyMode::Chunked;
  } else if (has_content_length) {
    if (content_remaining_ > body_.size()) {
      error = "HTTP body exceeds configured capacity";
      return false;
    }
    body_mode_ = BodyMode::ContentLength;
  } else {
    error = "HTTP response has no supported body framing";
    return false;
  }
  return true;
}

bool HttpResponseParser::append_body(std::span<const std::byte> bytes,
                                     std::string_view &error) noexcept {
  if (bytes.size() > body_.size() - body_used_) {
    error = "HTTP body exceeds configured capacity";
    return false;
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
          trailer_match_ = 0;
        } else {
          if (chunk_remaining_ > body_.size() - body_used_) {
            error = "HTTP body exceeds configured capacity";
            return false;
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
      const char value = static_cast<char>(bytes[position++]);
      ++chunk_line_used_;
      constexpr char terminal[4] = {'\r', '\n', '\r', '\n'};
      if (value == terminal[trailer_match_]) {
        ++trailer_match_;
      } else {
        trailer_match_ = value == '\r' ? 1U : 0U;
      }
      if ((chunk_line_used_ == 2 && trailer_match_ == 2) ||
          trailer_match_ == 4) {
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
  if (complete_) {
    if (!bytes.empty()) {
      error = "bytes received after HTTP response";
      return false;
    }
    return true;
  }
  if (headers_complete_) {
    return feed_body(bytes, error);
  }
  std::size_t position = 0;
  while (position < bytes.size()) {
    if (header_used_ == headers_.size()) {
      error = "HTTP headers exceed configured capacity";
      return false;
    }
    headers_[header_used_++] = bytes[position++];
    if (header_used_ >= 4 && headers_[header_used_ - 4] == std::byte{'\r'} &&
        headers_[header_used_ - 3] == std::byte{'\n'} &&
        headers_[header_used_ - 2] == std::byte{'\r'} &&
        headers_[header_used_ - 1] == std::byte{'\n'}) {
      if (!parse_headers(error)) {
        return false;
      }
      return complete_ ? position == bytes.size()
                       : feed_body(bytes.subspan(position), error);
    }
  }
  return true;
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
  request_size_ = 0;
  request_offset_ = 0;
  state_ = HttpClientState::Idle;
  wanted_events_ = 0;
  error_ = {};
}

void HttpClient::fail(std::string_view error) noexcept {
  state_ = HttpClientState::Failed;
  wanted_events_ = 0;
  error_ = error;
}

bool HttpClient::start_get(std::string_view host, std::string_view service,
                           std::string_view target,
                           Clock::time_point deadline) noexcept {
  reset();
  if (!context_ || host.empty() || host.size() + 1 > host_.size() ||
      target.empty()) {
    fail("invalid HTTPS GET parameters");
    return false;
  }
  std::memcpy(host_.data(), host.data(), host.size());
  host_[host.size()] = '\0';
  const auto append = [this](std::string_view value) noexcept {
    if (value.size() > request_.size() - request_size_) {
      return false;
    }
    std::memcpy(request_.data() + request_size_, value.data(), value.size());
    request_size_ += value.size();
    return true;
  };
  if (!append("GET ") || !append(target) || !append(" HTTP/1.1\r\nHost: ") ||
      !append(host) ||
      !append("\r\nAccept: application/json\r\nConnection: close\r\n"
              "User-Agent: self-quant-mds/1\r\n\r\n")) {
    fail("HTTPS request exceeds configured capacity");
    return false;
  }
  deadline_ = deadline;
  if (!connector_.start(host, service, deadline)) {
    fail(connector_.error_message());
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
    fail("failed to create TLS session");
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
      fail("TLS handshake failed");
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
      fail("HTTPS request write failed");
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
        fail(error_);
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
      fail("HTTPS connection closed before response completed");
      return;
    }
    wanted_events_ = static_cast<std::uint32_t>(tls_->wanted_events(result));
    if (wanted_events_ == EPOLLERR) {
      fail("HTTPS response read failed");
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
      fail(connector_.error_message());
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

} // namespace mds::network

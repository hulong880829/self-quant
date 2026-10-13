#include "net/http_parser.h"

#include <algorithm>
#include <cctype>
#include <charconv>
#include <cstring>

namespace net {
namespace {

bool ascii_iequals(std::string_view left, std::string_view right) noexcept {
  if (left.size() != right.size()) {
    return false;
  }
  for (std::size_t i = 0; i < left.size(); ++i) {
    unsigned char a = static_cast<unsigned char>(left[i]);
    unsigned char b = static_cast<unsigned char>(right[i]);
    if (a >= 'A' && a <= 'Z') {
      a = static_cast<unsigned char>(a + ('a' - 'A'));
    }
    if (b >= 'A' && b <= 'Z') {
      b = static_cast<unsigned char>(b + ('a' - 'A'));
    }
    if (a != b) {
      return false;
    }
  }
  return true;
}

std::string_view trim(std::string_view value) noexcept {
  while (!value.empty() &&
         (value.front() == ' ' || value.front() == '\t')) {
    value.remove_prefix(1);
  }
  while (!value.empty() && (value.back() == ' ' || value.back() == '\t')) {
    value.remove_suffix(1);
  }
  return value;
}

int hex_value(char c) noexcept {
  if (c >= '0' && c <= '9') {
    return c - '0';
  }
  if (c >= 'a' && c <= 'f') {
    return c - 'a' + 10;
  }
  if (c >= 'A' && c <= 'F') {
    return c - 'A' + 10;
  }
  return -1;
}

bool is_token_char(unsigned char c) noexcept {
  return std::isalnum(c) != 0 || std::strchr("!#$%&'*+-.^_`|~", c) != nullptr;
}

}  // namespace

bool percent_decode(std::string_view input, std::string &output,
                    bool plus_as_space) noexcept {
  output.clear();
  output.reserve(input.size());
  for (std::size_t i = 0; i < input.size(); ++i) {
    const char c = input[i];
    if (plus_as_space && c == '+') {
      output.push_back(' ');
      continue;
    }
    if (c != '%') {
      output.push_back(c);
      continue;
    }
    if (i + 2 >= input.size()) {
      return false;
    }
    const int high = hex_value(input[i + 1]);
    const int low = hex_value(input[i + 2]);
    if (high < 0 || low < 0) {
      return false;
    }
    output.push_back(static_cast<char>((high << 4) | low));
    i += 2;
  }
  return true;
}

std::string_view header_value(const HttpRequest &request,
                              std::string_view name) noexcept {
  for (const auto &header : request.headers) {
    if (ascii_iequals(header.first, name)) {
      return header.second;
    }
  }
  return {};
}

std::optional<std::string> query_value(std::string_view query,
                                       std::string_view key) noexcept {
  std::size_t position = 0;
  while (position <= query.size()) {
    const auto amp = query.find('&', position);
    const auto part = query.substr(
        position, amp == std::string_view::npos ? query.size() - position
                                                : amp - position);
    const auto eq = part.find('=');
    std::string name;
    std::string value;
    const auto raw_name =
        eq == std::string_view::npos ? part : part.substr(0, eq);
    const auto raw_value =
        eq == std::string_view::npos ? std::string_view{} : part.substr(eq + 1);
    if (!percent_decode(raw_name, name, true) ||
        !percent_decode(raw_value, value, true)) {
      return std::nullopt;
    }
    if (name == key) {
      return value;
    }
    if (amp == std::string_view::npos) {
      break;
    }
    position = amp + 1;
  }
  return std::nullopt;
}

HttpRequestParser::HttpRequestParser(HttpLimits limits) : limits_(limits) {
  buffer_.reserve(std::min(limits_.max_header_bytes, std::size_t{4096}));
}

void HttpRequestParser::reset() noexcept {
  buffer_.clear();
  parsed_ = 0;
  body_needed_ = 0;
  headers_done_ = false;
  complete_ = false;
  request_ = {};
  error_.clear();
}

HttpParseStatus HttpRequestParser::feed(std::span<const std::byte> bytes,
                                        std::size_t &consumed,
                                        std::string_view &error) noexcept {
  consumed = 0;
  error = {};
  if (complete_) {
    return HttpParseStatus::Complete;
  }
  if (bytes.empty()) {
    return HttpParseStatus::NeedMore;
  }
  const auto remaining_cap =
      headers_done_ ? limits_.max_header_bytes + limits_.max_body_bytes
                    : limits_.max_header_bytes;
  if (buffer_.size() + bytes.size() > remaining_cap + 2) {
    error_ = "request exceeds size limit";
    error = error_;
    return HttpParseStatus::Error;
  }
  buffer_.insert(buffer_.end(), bytes.begin(), bytes.end());
  if (!parse_available(error)) {
    return HttpParseStatus::Error;
  }
  consumed = bytes.size();
  if (complete_) {
    const auto extra = buffer_.size() - parsed_;
    if (extra > bytes.size()) {
      consumed = 0;
    } else {
      consumed = bytes.size() - extra;
      buffer_.resize(parsed_);
    }
    return HttpParseStatus::Complete;
  }
  if (!headers_done_ && buffer_.size() > limits_.max_header_bytes) {
    error_ = "request headers exceed size limit";
    error = error_;
    return HttpParseStatus::Error;
  }
  return HttpParseStatus::NeedMore;
}

bool HttpRequestParser::parse_available(std::string_view &error) noexcept {
  auto text = std::string_view(
      reinterpret_cast<const char *>(buffer_.data()), buffer_.size());
  while (!headers_done_) {
    const auto remaining = text.substr(parsed_);
    const auto end = remaining.find("\r\n");
    if (end == std::string_view::npos) {
      return true;
    }
    const auto line = remaining.substr(0, end);
    parsed_ += end + 2;
    if (request_.method.empty()) {
      if (!parse_request_line(line, error)) {
        return false;
      }
      continue;
    }
    if (line.empty()) {
      if (!finish_headers(error)) {
        return false;
      }
      break;
    }
    if (!parse_header_line(line, error)) {
      return false;
    }
  }
  if (!headers_done_) {
    return true;
  }
  if (body_needed_ == 0) {
    complete_ = true;
    return true;
  }
  if (buffer_.size() - parsed_ < body_needed_) {
    return true;
  }
  request_.body.assign(buffer_.begin() + static_cast<std::ptrdiff_t>(parsed_),
                       buffer_.begin() +
                           static_cast<std::ptrdiff_t>(parsed_ + body_needed_));
  parsed_ += body_needed_;
  complete_ = true;
  return true;
}

bool HttpRequestParser::parse_request_line(std::string_view line,
                                           std::string_view &error) noexcept {
  if (line.empty()) {
    error_ = "empty request line";
    error = error_;
    return false;
  }
  const auto sp1 = line.find(' ');
  const auto sp2 = sp1 == std::string_view::npos ? std::string_view::npos
                                                 : line.find(' ', sp1 + 1);
  if (sp1 == std::string_view::npos || sp2 == std::string_view::npos ||
      line.find(' ', sp2 + 1) != std::string_view::npos) {
    error_ = "malformed request line";
    error = error_;
    return false;
  }
  request_.method = std::string(line.substr(0, sp1));
  request_.target = std::string(line.substr(sp1 + 1, sp2 - sp1 - 1));
  request_.version = std::string(line.substr(sp2 + 1));
  if (request_.method != "GET" && request_.method != "OPTIONS") {
    error_ = "unsupported method";
    error = error_;
    return false;
  }
  if (request_.version != "HTTP/1.0" && request_.version != "HTTP/1.1") {
    error_ = "unsupported HTTP version";
    error = error_;
    return false;
  }
  if (request_.target.empty() || request_.target.front() != '/') {
    error_ = "request target must be an origin-form path";
    error = error_;
    return false;
  }
  const auto q = std::string_view(request_.target).find('?');
  const auto raw_path = q == std::string_view::npos
                            ? std::string_view(request_.target)
                            : std::string_view(request_.target).substr(0, q);
  const auto raw_query = q == std::string_view::npos
                             ? std::string_view{}
                             : std::string_view(request_.target).substr(q + 1);
  if (!percent_decode(raw_path, request_.path, false) ||
      request_.path.empty() || request_.path.front() != '/') {
    error_ = "invalid request path";
    error = error_;
    return false;
  }
  request_.query = std::string(raw_query);
  request_.keep_alive = request_.version == "HTTP/1.1";
  return true;
}

bool HttpRequestParser::parse_header_line(std::string_view line,
                                          std::string_view &error) noexcept {
  const auto colon = line.find(':');
  if (colon == std::string_view::npos || colon == 0) {
    error_ = "malformed header";
    error = error_;
    return false;
  }
  const auto name = line.substr(0, colon);
  for (const char c : name) {
    if (!is_token_char(static_cast<unsigned char>(c))) {
      error_ = "invalid header name";
      error = error_;
      return false;
    }
  }
  request_.headers.emplace_back(std::string(name),
                                std::string(trim(line.substr(colon + 1))));
  return true;
}

bool HttpRequestParser::finish_headers(std::string_view &error) noexcept {
  const auto connection = header_value(request_, "Connection");
  if (!connection.empty()) {
    if (ascii_iequals(connection, "close") ||
        connection.find("close") != std::string_view::npos) {
      // Tokenize below; this is only a fast path for the exact header.
    }
    bool saw_close = false;
    bool saw_keep_alive = false;
    std::size_t position = 0;
    while (position < connection.size()) {
      const auto comma = connection.find(',', position);
      const auto token = trim(connection.substr(
          position, comma == std::string_view::npos
                        ? connection.size() - position
                        : comma - position));
      if (ascii_iequals(token, "close")) {
        saw_close = true;
      }
      if (ascii_iequals(token, "keep-alive")) {
        saw_keep_alive = true;
      }
      if (comma == std::string_view::npos) {
        break;
      }
      position = comma + 1;
    }
    if (saw_close) {
      request_.keep_alive = false;
    } else if (saw_keep_alive) {
      request_.keep_alive = true;
    }
  }
  if (request_.version == "HTTP/1.0" && connection.empty()) {
    request_.keep_alive = false;
  }
  const auto content_length = header_value(request_, "Content-Length");
  if (!content_length.empty()) {
    std::uint64_t length = 0;
    const auto *begin = content_length.data();
    const auto *end = begin + content_length.size();
    const auto parsed = std::from_chars(begin, end, length);
    if (parsed.ec != std::errc{} || parsed.ptr != end) {
      error_ = "invalid Content-Length";
      error = error_;
      return false;
    }
    if (length > limits_.max_body_bytes) {
      error_ = "request body exceeds size limit";
      error = error_;
      return false;
    }
    body_needed_ = static_cast<std::size_t>(length);
  }
  const auto upgrade = header_value(request_, "Upgrade");
  request_.websocket_upgrade =
      ascii_iequals(upgrade, "websocket") &&
      !header_value(request_, "Sec-WebSocket-Key").empty();
  headers_done_ = true;
  return true;
}

void HttpResponseWriter::reset() noexcept {
  status_ = 200;
  reason_ = "OK";
  headers_.clear();
  content_type_ = "text/plain; charset=utf-8";
  body_.clear();
}

void HttpResponseWriter::set_status(int status,
                                    std::string_view reason) noexcept {
  status_ = status;
  reason_ = std::string(reason);
}

void HttpResponseWriter::set_header(std::string_view name,
                                    std::string_view value) {
  headers_.emplace_back(std::string(name), std::string(value));
}

void HttpResponseWriter::set_body(std::string_view content_type,
                                  std::string_view body) {
  content_type_ = std::string(content_type);
  body_ = std::string(body);
}

void HttpResponseWriter::set_body(std::string_view content_type,
                                  std::span<const std::byte> body) {
  content_type_ = std::string(content_type);
  body_.assign(reinterpret_cast<const char *>(body.data()), body.size());
}

std::string HttpResponseWriter::serialize(bool keep_alive) const {
  std::string output;
  output.reserve(128 + body_.size());
  output += "HTTP/1.1 ";
  output += std::to_string(status_);
  output += " ";
  output += reason_;
  output += "\r\n";
  auto write_header = [&](std::string_view name, std::string_view value) {
    output.append(name);
    output += ": ";
    output.append(value);
    output += "\r\n";
  };
  write_header("Content-Type", content_type_);
  write_header("Content-Length", std::to_string(body_.size()));
  write_header("Connection", keep_alive ? "keep-alive" : "close");
  for (const auto &header : headers_) {
    write_header(header.first, header.second);
  }
  output += "\r\n";
  output += body_;
  return output;
}

}  // namespace net

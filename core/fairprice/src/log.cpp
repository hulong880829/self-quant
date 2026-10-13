#include "fairprice/log.h"

#include <chrono>
#include <cstdarg>
#include <cstdint>
#include <cstdio>
#include <ctime>
#include <string>
#include <vector>

namespace fairprice {
namespace {

LogLevel g_min_level{LogLevel::Info};
std::string g_instance{"0-0"};
std::vector<std::string> *g_capture{nullptr};

void format_prefix(char *buffer, std::size_t size, LogLevel level) {
  const auto now = std::chrono::system_clock::now();
  const auto seconds = std::chrono::time_point_cast<std::chrono::seconds>(now);
  const auto millis = std::chrono::duration_cast<std::chrono::milliseconds>(
                          now - seconds)
                          .count();
  const auto time = std::chrono::system_clock::to_time_t(seconds);
  std::tm utc{};
#if defined(_WIN32)
  gmtime_s(&utc, &time);
#else
  gmtime_r(&time, &utc);
#endif
  std::snprintf(buffer, size,
                "%04d-%02d-%02dT%02d:%02d:%02d.%03dZ instance=%s level=%s "
                "fairprice: ",
                utc.tm_year + 1900, utc.tm_mon + 1, utc.tm_mday, utc.tm_hour,
                utc.tm_min, utc.tm_sec, static_cast<int>(millis),
                g_instance.c_str(), log_level_name(level));
}

void emit(LogLevel level, const char *fmt, va_list args) {
  if (!log_level_enabled(level)) {
    return;
  }
  char prefix[160]{};
  format_prefix(prefix, sizeof(prefix), level);
  char body[1024]{};
  std::vsnprintf(body, sizeof(body), fmt, args);
  std::fprintf(stderr, "%s%s\n", prefix, body);
  if (g_capture) {
    g_capture->emplace_back(std::string(prefix) + body);
  }
}

}  // namespace

bool parse_log_level(std::string_view text, LogLevel &out) noexcept {
  if (text == "debug") {
    out = LogLevel::Debug;
    return true;
  }
  if (text == "info") {
    out = LogLevel::Info;
    return true;
  }
  if (text == "warn") {
    out = LogLevel::Warn;
    return true;
  }
  if (text == "error") {
    out = LogLevel::Error;
    return true;
  }
  return false;
}

const char *log_level_name(LogLevel level) noexcept {
  switch (level) {
    case LogLevel::Debug:
      return "debug";
    case LogLevel::Info:
      return "info";
    case LogLevel::Warn:
      return "warn";
    case LogLevel::Error:
      return "error";
  }
  return "info";
}

bool log_level_enabled(LogLevel level) noexcept {
  return static_cast<int>(level) >= static_cast<int>(g_min_level);
}

void init_log(LogLevel min_level, std::string instance) {
  g_min_level = min_level;
  if (!instance.empty()) {
    g_instance = std::move(instance);
  }
}

void log_write(LogLevel level, const char *fmt, ...) {
  va_list args;
  va_start(args, fmt);
  emit(level, fmt, args);
  va_end(args);
}

void log_debug(const char *fmt, ...) {
  va_list args;
  va_start(args, fmt);
  emit(LogLevel::Debug, fmt, args);
  va_end(args);
}

void log_info(const char *fmt, ...) {
  va_list args;
  va_start(args, fmt);
  emit(LogLevel::Info, fmt, args);
  va_end(args);
}

void log_warn(const char *fmt, ...) {
  va_list args;
  va_start(args, fmt);
  emit(LogLevel::Warn, fmt, args);
  va_end(args);
}

void log_error(const char *fmt, ...) {
  va_list args;
  va_start(args, fmt);
  emit(LogLevel::Error, fmt, args);
  va_end(args);
}

void set_log_capture_for_test(std::vector<std::string> *lines) {
  g_capture = lines;
}

LogLevel current_log_level_for_test() noexcept { return g_min_level; }

const std::string &log_instance_for_test() noexcept { return g_instance; }

}  // namespace fairprice

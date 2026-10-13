#pragma once

#include <string>
#include <string_view>
#include <vector>

namespace fairprice {

enum class LogLevel : int { Debug = 0, Info = 1, Warn = 2, Error = 3 };

[[nodiscard]] bool parse_log_level(std::string_view text,
                                   LogLevel &out) noexcept;
[[nodiscard]] const char *log_level_name(LogLevel level) noexcept;
[[nodiscard]] bool log_level_enabled(LogLevel level) noexcept;

void init_log(LogLevel min_level, std::string instance);
void log_write(LogLevel level, const char *fmt, ...)
#if defined(__GNUC__)
    __attribute__((format(printf, 2, 3)))
#endif
    ;

void log_debug(const char *fmt, ...)
#if defined(__GNUC__)
    __attribute__((format(printf, 1, 2)))
#endif
    ;
void log_info(const char *fmt, ...)
#if defined(__GNUC__)
    __attribute__((format(printf, 1, 2)))
#endif
    ;
void log_warn(const char *fmt, ...)
#if defined(__GNUC__)
    __attribute__((format(printf, 1, 2)))
#endif
    ;
void log_error(const char *fmt, ...)
#if defined(__GNUC__)
    __attribute__((format(printf, 1, 2)))
#endif
    ;

void set_log_capture_for_test(std::vector<std::string> *lines);
[[nodiscard]] LogLevel current_log_level_for_test() noexcept;
[[nodiscard]] const std::string &log_instance_for_test() noexcept;

}  // namespace fairprice

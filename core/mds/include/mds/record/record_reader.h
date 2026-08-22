#pragma once

#include "mds/api/mds_api.h"
#include "mds/record/record.h"

#include <filesystem>
#include <functional>
#include <string>

namespace mds::record {

enum class ReadError {
  Ok,
  Io,
  Truncated,
  Corrupt,
  UnsupportedVersion,
  InvalidRecord,
};

struct ReadResult {
  ReadError error{ReadError::Ok};
  std::string message;
  std::uint64_t records{};

  [[nodiscard]] explicit operator bool() const noexcept {
    return error == ReadError::Ok;
  }
};

using RecordVisitor = std::function<bool(const Record &)>;

// Validates the compressed container and reconstructs only schema-defined
// fields. Reserved bytes and source-ABI padding are always zero in Record.
[[nodiscard]] ReadResult read_file(const std::filesystem::path &path,
                                   const RecordVisitor &visitor);

[[nodiscard]] ReadResult validate_file(const std::filesystem::path &path);

}  // namespace mds::record

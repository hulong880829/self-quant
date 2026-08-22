#pragma once

#include "mds/record/record.h"
#include "mds/record/record_reader.h"

#include <cstdint>
#include <span>
#include <vector>

namespace mds::record::detail {

inline constexpr std::uint32_t kContainerMagic = 0x43525153U;  // SQRC
inline constexpr std::uint32_t kFrameMagic = 0x46525153U;      // SQRF
inline constexpr std::uint32_t kTrailerMagic = 0x45525153U;    // SQRE

[[nodiscard]] std::uint32_t crc32(std::span<const std::uint8_t> bytes) noexcept;
[[nodiscard]] std::vector<std::uint8_t> container_header();
[[nodiscard]] std::vector<std::uint8_t> encode_record(const Record &record);
[[nodiscard]] std::vector<std::uint8_t> container_trailer(
    std::uint64_t records);
[[nodiscard]] ReadResult decode_container(
    std::span<const std::uint8_t> bytes, const RecordVisitor &visitor);

}  // namespace mds::record::detail

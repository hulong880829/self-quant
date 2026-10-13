#pragma once

#include "fairprice/json.h"

#include <cstddef>
#include <cstdint>
#include <memory>
#include <string_view>
#include <vector>

namespace fairprice {

inline constexpr std::uint32_t kSqabMagic = 0x42415153U;
inline constexpr std::uint8_t kSqabKindBbo = 1;
inline constexpr std::uint8_t kSqabKindBook = 2;

std::shared_ptr<const std::vector<std::byte>>
encode_sqab_bbo(std::string_view symbol, const BboView &bbo);
std::shared_ptr<const std::vector<std::byte>>
encode_sqab_book(std::string_view symbol, const BookSnapshot &book, int depth);

}  // namespace fairprice

#pragma once

#include <algorithm>
#include <array>
#include <cstdint>
#include <vector>

#include "oms/api/error.h"
#include "oms/api/order_types.h"
#include "utils/md/symbol.h"

namespace oms {

enum class PolymarketOutcome : std::uint8_t { Unknown = 0, Yes = 1, No = 2 };
enum class MetadataKind : std::uint8_t { Generic = 0, Polymarket = 1 };

struct PolymarketMetadata {
  std::array<std::uint8_t, 32> condition_id{};
  std::array<std::uint8_t, 32> token_id{};
  PolymarketOutcome outcome{PolymarketOutcome::Unknown};
  bool negative_risk{};
  std::uint8_t signature_type{};
  std::uint8_t reserved{};
  std::int64_t minimum_order_size{};
  std::uint32_t taker_delay_ms{};
};

struct TradingMetadata {
  api::InstrumentId instrument_id{};
  MetadataKind kind{MetadataKind::Generic};
  std::array<std::uint8_t, 3> reserved{};
  PolymarketMetadata polymarket{};
};

class InstrumentRegistry {
 public:
  api::Error Add(const utils::md::Instrument& instrument,
                 const TradingMetadata* trading = nullptr) {
    if (frozen_) return api::Error::Frozen;
    if (instrument.venue == utils::md::Venue::Unknown ||
        instrument.product_type == utils::md::ProductType::Unknown) {
      return api::Error::InvalidArgument;
    }
    if (instrument.price_scale > 18 || instrument.quantity_scale > 18 ||
        instrument.contract_multiplier_scale > 18 ||
        instrument.contract_multiplier < 0) {
      return api::Error::InvalidScale;
    }
    if (instruments_.Find(instrument.instrument_id) != nullptr)
      return api::Error::Duplicate;
    if (trading != nullptr &&
        (trading->instrument_id != instrument.instrument_id ||
         HasTradingMetadata(trading->instrument_id))) {
      return api::Error::Conflict;
    }
    if (trading != nullptr &&
        instrument.venue == utils::md::Venue::Polymarket) {
      if (trading->kind != MetadataKind::Polymarket)
        return api::Error::InvalidArgument;
      const auto& metadata = trading->polymarket;
      const bool condition_empty =
          std::all_of(metadata.condition_id.begin(), metadata.condition_id.end(),
                      [](std::uint8_t value) { return value == 0; });
      const bool token_empty =
          std::all_of(metadata.token_id.begin(), metadata.token_id.end(),
                      [](std::uint8_t value) { return value == 0; });
      const bool signature_valid = metadata.signature_type == 0 ||
                                   metadata.signature_type == 3;
      if (condition_empty || token_empty ||
          metadata.outcome == PolymarketOutcome::Unknown ||
          !signature_valid || metadata.minimum_order_size <= 0) {
        return api::Error::InvalidArgument;
      }
    } else if (trading != nullptr) {
      const auto& metadata = trading->polymarket;
      const bool condition_empty =
          std::all_of(metadata.condition_id.begin(), metadata.condition_id.end(),
                      [](std::uint8_t value) { return value == 0; });
      const bool token_empty =
          std::all_of(metadata.token_id.begin(), metadata.token_id.end(),
                      [](std::uint8_t value) { return value == 0; });
      if (trading->kind != MetadataKind::Generic || !condition_empty ||
          !token_empty ||
          metadata.outcome != PolymarketOutcome::Unknown ||
          metadata.negative_risk || metadata.signature_type != 0 ||
          metadata.minimum_order_size != 0 ||
          metadata.taker_delay_ms != 0) {
        return api::Error::InvalidArgument;
      }
    }
    const auto result = instruments_.Register(instrument);
    if (result != utils::md::RegistryResult::Ok) {
      return result == utils::md::RegistryResult::Invalid
                 ? api::Error::InvalidArgument
                 : api::Error::Duplicate;
    }
    if (trading != nullptr) {
      trading_.push_back(*trading);
    }
    return api::Error::Ok;
  }

  api::Error Freeze() {
    if (frozen_) return api::Error::Frozen;
    std::sort(trading_.begin(), trading_.end(),
              [](const TradingMetadata& lhs, const TradingMetadata& rhs) {
                return lhs.instrument_id < rhs.instrument_id;
              });
    frozen_ = true;
    return api::Error::Ok;
  }

  [[nodiscard]] bool frozen() const noexcept { return frozen_; }
  [[nodiscard]] std::size_t size() const noexcept { return instruments_.size(); }

  [[nodiscard]] const utils::md::Instrument* Find(
      api::InstrumentId instrument_id) const noexcept {
    return instruments_.Find(instrument_id);
  }

  [[nodiscard]] const TradingMetadata* FindTradingMetadata(
      api::InstrumentId instrument_id) const noexcept {
    if (!frozen_) {
      const auto it = std::find_if(
          trading_.begin(), trading_.end(),
          [instrument_id](const TradingMetadata& value) {
            return value.instrument_id == instrument_id;
          });
      return it == trading_.end() ? nullptr : &*it;
    }
    const auto it = std::lower_bound(
        trading_.begin(), trading_.end(), instrument_id,
        [](const TradingMetadata& value, api::InstrumentId id) {
          return value.instrument_id < id;
        });
    return it != trading_.end() && it->instrument_id == instrument_id ? &*it
                                                                       : nullptr;
  }

  [[nodiscard]] api::InstrumentId FindPolymarketToken(
      const std::array<std::uint8_t, 32>& token_id) const noexcept {
    const auto found = std::find_if(
        trading_.begin(), trading_.end(), [&](const TradingMetadata& value) {
          return value.kind == MetadataKind::Polymarket &&
                 value.polymarket.token_id == token_id;
        });
    return found == trading_.end() ? 0 : found->instrument_id;
  }

  [[nodiscard]] api::Error ValidateRebind(
      const api::RebindPolymarketInstrumentRequest& request) const noexcept {
    const auto* instrument = Find(request.instrument_id);
    const auto* current = FindTradingMetadata(request.instrument_id);
    if (!frozen_ || instrument == nullptr || current == nullptr ||
        instrument->venue != utils::md::Venue::Polymarket ||
        current->kind != MetadataKind::Polymarket) {
      return api::Error::InvalidArgument;
    }
    const bool condition_empty =
        std::all_of(request.condition_id.begin(), request.condition_id.end(),
                    [](std::uint8_t value) { return value == 0; });
    const bool token_empty =
        std::all_of(request.token_id.begin(), request.token_id.end(),
                    [](std::uint8_t value) { return value == 0; });
    if (condition_empty || token_empty ||
        (request.outcome != api::PolymarketOutcome::Yes &&
         request.outcome != api::PolymarketOutcome::No) ||
        (request.signature_type != 0 && request.signature_type != 3) ||
        request.minimum_order_size <= 0) {
      return api::Error::InvalidArgument;
    }
    return api::Error::Ok;
  }

  api::Error Rebind(
      const api::RebindPolymarketInstrumentRequest& request) noexcept {
    const api::Error valid = ValidateRebind(request);
    if (valid != api::Error::Ok) return valid;
    auto* current = const_cast<TradingMetadata*>(
        FindTradingMetadata(request.instrument_id));
    current->polymarket.condition_id = request.condition_id;
    current->polymarket.token_id = request.token_id;
    current->polymarket.outcome =
        request.outcome == api::PolymarketOutcome::Yes
            ? PolymarketOutcome::Yes
            : PolymarketOutcome::No;
    current->polymarket.negative_risk = request.negative_risk;
    current->polymarket.signature_type = request.signature_type;
    current->polymarket.minimum_order_size = request.minimum_order_size;
    current->polymarket.taker_delay_ms = request.taker_delay_ms;
    return api::Error::Ok;
  }

  [[nodiscard]] bool IsTradeable(api::InstrumentId instrument_id) const noexcept {
    return Find(instrument_id) != nullptr &&
           FindTradingMetadata(instrument_id) != nullptr;
  }

 private:
  [[nodiscard]] bool HasTradingMetadata(api::InstrumentId instrument_id) const {
    return std::any_of(trading_.begin(), trading_.end(),
                       [instrument_id](const TradingMetadata& value) {
                         return value.instrument_id == instrument_id;
                       });
  }

  utils::md::InstrumentRegistry instruments_;
  std::vector<TradingMetadata> trading_;
  bool frozen_{};
};

}  // namespace oms

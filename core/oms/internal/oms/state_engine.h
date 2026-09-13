#pragma once

#include <cstddef>
#include <vector>

#include "oms/order_table.h"

namespace oms {

class StateEngine {
 public:
  StateEngine(OrderTable& orders, std::size_t fill_dedup_capacity);

  api::Error Submit(const api::SubmitOrderRequest& request,
                    api::OrderHandle& handle,
                    const api::UpdateSink& sink) noexcept;
  api::Error RejectLocal(api::OrderHandle handle,
                         const api::UpdateSink& sink) noexcept;
  api::Error RequestCancel(api::RequestToken token,
                           const api::UpdateSink& sink) noexcept;
  api::Error Apply(const api::VenueEvent& event,
                   const api::UpdateSink& sink) noexcept;

 private:
  struct FillDedupEntry {
    api::OrderHandle handle{};
    api::TradeId trade_id{};
    std::int64_t quantity{};
    std::int64_t price{};
    std::uint8_t state{};
  };

  [[nodiscard]] api::Error Resolve(const api::VenueEvent& event,
                                   OrderRecord*& record) noexcept;
  [[nodiscard]] api::Error CheckDuplicate(
      api::OrderHandle handle, const api::VenueEvent& event,
      std::size_t& insert_slot) const noexcept;
  void RecordFill(std::size_t slot, api::OrderHandle handle,
                  const api::VenueEvent& event) noexcept;
  static void EmitOrder(api::UpdateType type, api::OrderHandle handle,
                        const OrderRecord& record,
                        const api::UpdateSink& sink) noexcept;

  OrderTable& orders_;
  std::vector<FillDedupEntry> fill_dedup_;
};

}  // namespace oms

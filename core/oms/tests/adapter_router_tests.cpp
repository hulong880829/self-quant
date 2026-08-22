#include <stdexcept>
#include <string>

#include "oms/exchange/adapter_router.h"

namespace {

#define REQUIRE(value)                                                        \
  do {                                                                        \
    if (!(value))                                                             \
      throw std::runtime_error(std::string("require failed: ") + #value);     \
  } while (false)

class StubAdapter final : public oms::exchange::TradeAdapter {
 public:
  explicit StubAdapter(oms::exchange::AdapterIdentity identity)
      : identity_(identity) {}

  oms::exchange::AdapterIdentity identity() const noexcept override {
    return identity_;
  }
  oms::exchange::AdapterStatus status() const noexcept override {
    return oms::exchange::AdapterStatus::Ready;
  }
  oms::exchange::AdapterCapabilities capabilities() const noexcept override {
    return {};
  }
  oms::exchange::AdapterResult reserve_command(
      oms::exchange::AdapterCommandKind,
      oms::exchange::AdapterReservation&) noexcept override {
    return oms::exchange::AdapterResult::WouldBlock;
  }
  void cancel_reservation(
      oms::exchange::AdapterReservation) noexcept override {}
  oms::exchange::AdapterResult commit_place(
      oms::exchange::AdapterReservation,
      const oms::exchange::AdapterPlaceCommand&) noexcept override {
    return oms::exchange::AdapterResult::StaleReservation;
  }
  oms::exchange::AdapterResult commit_cancel(
      oms::exchange::AdapterReservation,
      const oms::exchange::AdapterCancelCommand&) noexcept override {
    return oms::exchange::AdapterResult::StaleReservation;
  }
  oms::exchange::AdapterServiceResult service_io(
      std::uint64_t, std::uint32_t,
      const oms::exchange::AdapterEventSink&) noexcept override {
    return {};
  }
  oms::exchange::AdapterResult on_deadline(
      const oms::exchange::AdapterDeadline&, std::uint64_t,
      const oms::exchange::AdapterEventSink&) noexcept override {
    return oms::exchange::AdapterResult::Unsupported;
  }
  oms::exchange::AdapterResult begin_reconcile(
      std::uint64_t, std::uint64_t,
      const oms::exchange::AdapterEventSink&) noexcept override {
    return oms::exchange::AdapterResult::Ok;
  }
  oms::exchange::AdapterResult shutdown(
      std::uint64_t,
      const oms::exchange::AdapterEventSink&) noexcept override {
    return oms::exchange::AdapterResult::Ok;
  }

 private:
  oms::exchange::AdapterIdentity identity_{};
};

utils::md::Instrument Instrument(utils::md::Venue venue,
                                 utils::md::ProductType product) {
  utils::md::Instrument result{};
  result.instrument_id = 1;
  result.venue = venue;
  result.product_type = product;
  return result;
}

}  // namespace

int main() {
  using namespace oms::exchange;
  StubAdapter spot(
      {AdapterKind::BinanceSpot, 0, utils::md::Venue::Binance,
       utils::md::ProductType::Spot, {}});
  StubAdapter usdm(
      {AdapterKind::BinanceUsdm, 0, utils::md::Venue::Binance,
       utils::md::ProductType::Perpetual, {}});
  StubAdapter polymarket(
      {AdapterKind::Polymarket, 0, utils::md::Venue::Polymarket,
       utils::md::ProductType::BinaryOption, {}});
  StubAdapter fake({AdapterKind::Fake, 0, utils::md::Venue::Unknown,
                    utils::md::ProductType::Unknown, {}});
  AdapterRouter router;
  REQUIRE(router.add(spot) == AdapterResult::Ok);
  REQUIRE(router.add(usdm) == AdapterResult::Ok);
  REQUIRE(router.add(polymarket) == AdapterResult::Ok);
  REQUIRE(router.add(fake) == AdapterResult::Ok);
  REQUIRE(router.add(spot) == AdapterResult::InvalidArgument);
  REQUIRE(router.route(
              Instrument(utils::md::Venue::Binance,
                         utils::md::ProductType::Spot)) == &spot);
  REQUIRE(router.route(
              Instrument(utils::md::Venue::Binance,
                         utils::md::ProductType::Future)) == &usdm);
  REQUIRE(router.route(
              Instrument(utils::md::Venue::Polymarket,
                         utils::md::ProductType::BinaryOption)) == &polymarket);
  REQUIRE(router.route(
              Instrument(utils::md::Venue::Okx,
                         utils::md::ProductType::Spot)) == &fake);
  REQUIRE(router.route(
              Instrument(utils::md::Venue::Unknown,
                         utils::md::ProductType::Spot)) == nullptr);
}

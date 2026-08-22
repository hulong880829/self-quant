#include "oms/runtime/replay_script.h"

#include <cstring>
#include <fstream>
#include <sstream>
#include <string>

namespace oms::runtime {

api::Result<ReplayScript> ReplayScript::Load(std::string_view path) {
  std::ifstream input{std::string(path)};
  if (!input) return {{}, api::Error::NotFound};
  ReplayScript result;
  std::string line;
  while (std::getline(input, line)) {
    if (line.empty() || line[0] == '#') continue;
    std::istringstream fields(line);
    std::string operation;
    api::ReplayStep step{};
    if (!(fields >> step.delay_ns >> operation))
      return {{}, api::Error::InvalidArgument};
    if (operation == "DISCONNECT") {
      step.control = api::ReplayControl::Disconnect;
    } else if (operation == "RECONNECT") {
      step.control = api::ReplayControl::Reconnect;
    } else {
      std::uint32_t lane = 0;
      std::uint32_t epoch = 0;
      std::uint64_t sequence = 0;
      if (!(fields >> lane >> epoch >> sequence))
        return {{}, api::Error::InvalidArgument};
      step.control = api::ReplayControl::Event;
      step.event.token = {lane, epoch, sequence};
      if (operation == "ACK") {
        step.event.type = api::VenueEventType::NewAck;
      } else if (operation == "EXPIRE") {
        step.event.type = api::VenueEventType::Expire;
      } else if (operation == "FILL") {
        std::string trade;
        unsigned int quantity_scale = 0;
        unsigned int price_scale = 0;
        if (!(fields >> trade >> step.event.fill_quantity.value >>
              quantity_scale >> step.event.fill_price.value >> price_scale) ||
            trade.empty() ||
            trade.size() > step.event.trade_id.value.size() ||
            quantity_scale > 18 || price_scale > 18)
          return {{}, api::Error::InvalidArgument};
        step.event.fill_quantity.scale =
            static_cast<std::uint8_t>(quantity_scale);
        step.event.fill_price.scale = static_cast<std::uint8_t>(price_scale);
        step.event.type = api::VenueEventType::Fill;
        std::memcpy(step.event.trade_id.value.data(), trade.data(),
                    trade.size());
        step.event.trade_id.length =
            static_cast<std::uint16_t>(trade.size());
      } else {
        return {{}, api::Error::InvalidArgument};
      }
    }
    result.steps_.push_back(step);
  }
  return {std::move(result), api::Error::Ok};
}

}  // namespace oms::runtime

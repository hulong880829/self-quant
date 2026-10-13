#include "mds/ibkr/ibkr_client.h"

namespace mds::ibkr {

IbkrErrorClass classify_ibkr_error(int code) noexcept {
  switch (code) {
    case 502:
    case 504:
    case 507:
    case 1100:
    case 1300:
    case 2110:
      return IbkrErrorClass::ConnectionLost;
    case 1101:
      return IbkrErrorClass::ConnectionRestoredDataLost;
    case 1102:
      return IbkrErrorClass::ConnectionRestoredDataMaintained;
    case 200:
    case 321:
    case 366:
      return IbkrErrorClass::Contract;
    case 322:
    case 354:
    case 10089:
    case 10090:
    case 10167:
    case 10168:
    case 10190:
    case 10197:
      return IbkrErrorClass::MarketDataDenied;
    case 2103:
    case 2104:
    case 2105:
    case 2106:
    case 2107:
    case 2108:
    case 2119:
    case 2157:
    case 2158:
      return IbkrErrorClass::Informational;
    default:
      return IbkrErrorClass::Ignored;
  }
}

}  // namespace mds::ibkr

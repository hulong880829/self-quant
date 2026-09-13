cmake_minimum_required(VERSION 3.25)
project(poly_mm LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 23)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

find_package(self_quant CONFIG REQUIRED)
find_package(simdjson CONFIG REQUIRED)

add_library(poly_mm_lib
  src/catalog_rollover.cpp
  src/fairprice_client.cpp
  src/market_window.cpp
  src/order_lifecycle.cpp
  src/pnl_ledger.cpp
  src/signal.cpp
  src/strategy.cpp
)
target_include_directories(poly_mm_lib PUBLIC include)
target_link_libraries(poly_mm_lib
  PUBLIC
    self_quant::strategyframe
    self_quant::net
    self_quant::utils
  PRIVATE
    simdjson::simdjson
)
target_compile_options(poly_mm_lib PRIVATE
  -Wall -Wextra -Wpedantic -Wconversion -Werror
)

add_executable(poly-mm src/main.cpp)
target_link_libraries(poly-mm PRIVATE poly_mm_lib)

add_executable(poly-mm-latency-probe src/latency_probe.cpp)
target_link_libraries(poly-mm-latency-probe PRIVATE poly_mm_lib)

include(CTest)
if(BUILD_TESTING)
  add_executable(poly_mm_tests tests/poly_mm_tests.cpp)
  target_link_libraries(poly_mm_tests PRIVATE poly_mm_lib)
  add_test(NAME poly_mm_tests COMMAND poly_mm_tests)
endif()

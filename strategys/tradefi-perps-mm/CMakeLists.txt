cmake_minimum_required(VERSION 3.24)
project(tradefi_perps_mm LANGUAGES CXX)
set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)

find_package(self_quant CONFIG REQUIRED)

add_library(tradefi_perps_mm STATIC
  src/normalize.cpp
  src/engine.cpp
  src/recovery.cpp
  src/strategy.cpp)
target_include_directories(tradefi_perps_mm PUBLIC include)
target_link_libraries(tradefi_perps_mm PUBLIC self_quant::strategyframe)
target_compile_options(tradefi_perps_mm PRIVATE -Wall -Wextra -Wpedantic)

add_executable(tradefi-perps-mm src/main.cpp)
target_link_libraries(tradefi-perps-mm PRIVATE tradefi_perps_mm)

add_executable(tradefi_normalize_tests tests/normalize_tests.cpp)
target_link_libraries(tradefi_normalize_tests PRIVATE tradefi_perps_mm)
add_executable(tradefi_engine_tests tests/engine_tests.cpp)
target_link_libraries(tradefi_engine_tests PRIVATE tradefi_perps_mm)
add_executable(tradefi_recovery_strategy_tests
  tests/recovery_strategy_tests.cpp)
target_link_libraries(tradefi_recovery_strategy_tests
  PRIVATE tradefi_perps_mm)

enable_testing()
add_test(NAME tradefi_normalize_tests COMMAND tradefi_normalize_tests)
add_test(NAME tradefi_engine_tests COMMAND tradefi_engine_tests)
add_test(NAME tradefi_recovery_strategy_tests
  COMMAND tradefi_recovery_strategy_tests)
add_test(NAME tradefi_shadow
  COMMAND tradefi-perps-mm
          ${CMAKE_CURRENT_SOURCE_DIR}/config/tradefi-perps-mm.example.yaml
          --validate-only)

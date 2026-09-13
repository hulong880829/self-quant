cmake_minimum_required(VERSION 3.20)
project(self_quant_oms VERSION 0.1.0 LANGUAGES CXX)

option(OMS_BUILD_TESTS "Build OMS unit tests" ON)
option(OMS_BUILD_ACCEPTANCE "Build credential-safe OMS acceptance tool" ON)

add_library(self_quant_oms STATIC
  internal/oms/exchange/adapter_router.cpp
  internal/oms/exchange/binance/protocol.cpp
  internal/oms/exchange/binance/trade_adapter.cpp
  internal/oms/exchange/fake_trade_adapter.cpp
  internal/oms/exchange/live_transport.cpp
  internal/oms/exchange/polymarket/crypto.cpp
  internal/oms/exchange/polymarket/protocol.cpp
  internal/oms/exchange/polymarket/trade_adapter.cpp
  internal/oms/exchange/rest_request_pool.cpp
  internal/oms/runtime/deadline_scheduler.cpp
  internal/oms/runtime/event_notifier.cpp
  internal/oms/runtime/replay_script.cpp
  internal/oms/runtime/timer_notifier.cpp
  src/execution_directory.cpp
  src/execution_channel.cpp
  src/oms_api.cpp
  src/order_table.cpp
  src/state_engine.cpp)
add_library(self_quant::oms ALIAS self_quant_oms)
set_target_properties(self_quant_oms PROPERTIES EXPORT_NAME oms)
target_compile_features(self_quant_oms PUBLIC cxx_std_20)
target_include_directories(self_quant_oms
  PUBLIC
    $<BUILD_INTERFACE:${CMAKE_CURRENT_SOURCE_DIR}/include>
  PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/internal)
target_link_libraries(self_quant_oms
  PUBLIC self_quant::utils
  PRIVATE self_quant::net)
target_compile_options(self_quant_oms PRIVATE -Wall -Wextra -Wpedantic)
if(TARGET self_quant_warnings)
  target_link_libraries(self_quant_oms PRIVATE self_quant_warnings)
endif()

if(OMS_BUILD_ACCEPTANCE)
  add_executable(oms_acceptance cmd/oms_acceptance/main.cpp)
  target_compile_features(oms_acceptance PRIVATE cxx_std_20)
  target_compile_options(oms_acceptance PRIVATE -Wall -Wextra -Wpedantic)
endif()

add_executable(oms_benchmark cmd/oms_benchmark/main.cpp)
target_link_libraries(oms_benchmark PRIVATE self_quant_oms)
target_compile_features(oms_benchmark PRIVATE cxx_std_20)
target_compile_options(oms_benchmark PRIVATE -Wall -Wextra -Wpedantic)

add_executable(oms_strategy_frame_example
  examples/strategy_frame_execution.cpp)
target_link_libraries(oms_strategy_frame_example PRIVATE self_quant_oms)
target_compile_features(oms_strategy_frame_example PRIVATE cxx_std_20)
target_compile_options(oms_strategy_frame_example PRIVATE
  -Wall -Wextra -Wpedantic)

if(OMS_BUILD_TESTS)
  enable_testing()
  add_executable(oms_core_tests tests/oms_core_tests.cpp)
  target_include_directories(oms_core_tests PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/internal)
  target_link_libraries(oms_core_tests PRIVATE self_quant_oms)
  target_compile_definitions(oms_core_tests PRIVATE
    OMS_TEST_FIXTURE_DIR="${CMAKE_CURRENT_SOURCE_DIR}/tests/fixtures")
  add_test(NAME oms_core_tests COMMAND oms_core_tests)

  add_executable(oms_fixed_spsc_ring_tests tests/fixed_spsc_ring_tests.cpp)
  target_include_directories(oms_fixed_spsc_ring_tests PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/internal)
  target_link_libraries(oms_fixed_spsc_ring_tests PRIVATE self_quant_oms)
  target_compile_options(oms_fixed_spsc_ring_tests PRIVATE
    $<$<CONFIG:Release>:-UNDEBUG>)
  add_test(NAME oms_fixed_spsc_ring_tests COMMAND oms_fixed_spsc_ring_tests)

  add_executable(oms_runtime_primitives_tests
    tests/runtime_primitives_tests.cpp)
  target_include_directories(oms_runtime_primitives_tests PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/internal)
  target_link_libraries(oms_runtime_primitives_tests PRIVATE self_quant_oms)
  target_compile_options(oms_runtime_primitives_tests PRIVATE
    $<$<CONFIG:Release>:-UNDEBUG>)
  add_test(NAME oms_runtime_primitives_tests
    COMMAND oms_runtime_primitives_tests)

  add_executable(oms_runtime_tests tests/oms_runtime_tests.cpp)
  target_include_directories(oms_runtime_tests PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/internal)
  target_link_libraries(oms_runtime_tests PRIVATE self_quant_oms)
  add_test(NAME oms_runtime_tests COMMAND oms_runtime_tests)

  add_executable(oms_adapter_router_tests
    tests/adapter_router_tests.cpp)
  target_include_directories(oms_adapter_router_tests PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/internal)
  target_link_libraries(oms_adapter_router_tests PRIVATE self_quant_oms)
  add_test(NAME oms_adapter_router_tests
    COMMAND oms_adapter_router_tests)

  add_executable(oms_rest_request_pool_tests
    tests/rest_request_pool_tests.cpp)
  target_include_directories(oms_rest_request_pool_tests PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/internal)
  target_link_libraries(oms_rest_request_pool_tests PRIVATE self_quant_oms)
  target_compile_options(oms_rest_request_pool_tests PRIVATE
    $<$<CONFIG:Release>:-UNDEBUG>)
  add_test(NAME oms_rest_request_pool_tests
    COMMAND oms_rest_request_pool_tests)

  add_executable(oms_live_transport_tests
    tests/live_transport_tests.cpp)
  target_include_directories(oms_live_transport_tests PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/internal)
  target_link_libraries(oms_live_transport_tests PRIVATE
    self_quant_oms
    self_quant::net)
  add_test(NAME oms_live_transport_tests
    COMMAND oms_live_transport_tests)

  add_executable(oms_binance_adapter_tests
    tests/binance_adapter_tests.cpp)
  target_include_directories(oms_binance_adapter_tests PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/internal)
  target_compile_definitions(oms_binance_adapter_tests PRIVATE
    OMS_TEST_FIXTURE_DIR="${CMAKE_CURRENT_SOURCE_DIR}/tests/fixtures")
  target_link_libraries(oms_binance_adapter_tests PRIVATE self_quant_oms)
  add_test(NAME oms_binance_adapter_tests
    COMMAND oms_binance_adapter_tests)

  add_executable(oms_polymarket_adapter_tests
    tests/polymarket_adapter_tests.cpp)
  target_include_directories(oms_polymarket_adapter_tests PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/internal)
  target_compile_definitions(oms_polymarket_adapter_tests PRIVATE
    OMS_TEST_FIXTURE_DIR="${CMAKE_CURRENT_SOURCE_DIR}/tests/fixtures")
  target_link_libraries(oms_polymarket_adapter_tests PRIVATE self_quant_oms)
  add_test(NAME oms_polymarket_adapter_tests
    COMMAND oms_polymarket_adapter_tests)

  add_executable(oms_dual_mode_tests tests/oms_dual_mode_tests.cpp)
  target_include_directories(oms_dual_mode_tests PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/internal)
  target_compile_definitions(oms_dual_mode_tests PRIVATE
    OMS_REPLAY_FIXTURE_DIR="${CMAKE_CURRENT_SOURCE_DIR}/tests/fixtures/replay")
  target_link_libraries(oms_dual_mode_tests PRIVATE self_quant_oms)
  add_test(NAME oms_dual_mode_tests COMMAND oms_dual_mode_tests)
endif()

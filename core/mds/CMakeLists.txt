cmake_minimum_required(VERSION 3.20)
project(selfquant_mds VERSION 0.1.0 LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

option(MDS_BUILD_TESTS "Build MDS unit tests" ON)
option(MDS_BUILD_EXAMPLES "Build MDS examples" ON)
option(MDS_ENABLE_SIMDJSON "Enable simdjson OnDemand JSON parsing" ON)

find_package(OpenSSL REQUIRED)
find_package(Threads REQUIRED)
if(MDS_ENABLE_SIMDJSON)
  find_package(simdjson CONFIG QUIET)
endif()

add_library(mds STATIC
  src/book/bbo_overlay.cpp
  src/book/book_bridge.cpp
  src/publish/wire_publisher.cpp
  src/service/binance_session.cpp
  src/service/mds_service.cpp
  src/transport/shared_ring.cpp
  src/network/epoll_loop.cpp
  src/network/http_client.cpp
  src/network/tcp_connector.cpp
  src/network/tls_websocket.cpp
  src/network/websocket_client.cpp
  src/network/websocket_codec.cpp
  src/exchange/binance/binance_adapter.cpp
  src/exchange/binance/binance_rest.cpp
  src/exchange/binance/binance_streams.cpp
  src/redundancy/sequence_arbiter.cpp
)
add_library(self_quant::mds ALIAS mds)
set_target_properties(mds PROPERTIES EXPORT_NAME mds)
target_include_directories(mds
  PUBLIC
    $<BUILD_INTERFACE:${CMAKE_CURRENT_SOURCE_DIR}/include>
    $<INSTALL_INTERFACE:${CMAKE_INSTALL_INCLUDEDIR}>
  PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/internal
)
target_link_libraries(mds
  PUBLIC
    self_quant::utils
    OpenSSL::SSL
    OpenSSL::Crypto
    Threads::Threads)
target_compile_options(mds PRIVATE -Wall -Wextra -Wpedantic)
if(TARGET self_quant_warnings)
  target_link_libraries(mds PRIVATE self_quant_warnings)
endif()

if(TARGET simdjson::simdjson)
  set(SELF_QUANT_HAS_SIMDJSON ON PARENT_SCOPE)
  target_link_libraries(mds PUBLIC simdjson::simdjson)
  target_compile_definitions(mds PUBLIC MDS_HAS_SIMDJSON=1)
else()
  set(SELF_QUANT_HAS_SIMDJSON OFF PARENT_SCOPE)
  message(STATUS
    "simdjson not found: Binance JSON parser returns a clear runtime error; "
    "install simdjson or configure with -DMDS_ENABLE_SIMDJSON=OFF")
endif()

if(MDS_BUILD_TESTS)
  enable_testing()
  add_executable(mds_tests tests/mds_tests.cpp)
  target_link_libraries(mds_tests PRIVATE mds)
  target_include_directories(mds_tests PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/internal)
  target_compile_definitions(mds_tests PRIVATE
    MDS_TEST_FIXTURE_DIR="${CMAKE_CURRENT_SOURCE_DIR}/tests/fixtures")
  add_test(NAME mds_tests COMMAND mds_tests)
  add_executable(mds_network_tests tests/network_tests.cpp)
  target_link_libraries(mds_network_tests PRIVATE mds)
  target_include_directories(mds_network_tests PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/internal)
  add_test(NAME mds_network_tests COMMAND mds_network_tests)
  add_executable(mds_wire_publisher_tests tests/wire_publisher_tests.cpp)
  target_link_libraries(mds_wire_publisher_tests PRIVATE mds)
  add_test(NAME mds_wire_publisher_tests COMMAND mds_wire_publisher_tests)
  add_executable(mds_binance_loopback_integration_tests
    tests/binance_loopback_integration_tests.cpp)
  target_link_libraries(mds_binance_loopback_integration_tests PRIVATE mds)
  target_include_directories(mds_binance_loopback_integration_tests PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/internal)
  target_compile_definitions(mds_binance_loopback_integration_tests PRIVATE
    MDS_TEST_FIXTURE_DIR="${CMAKE_CURRENT_SOURCE_DIR}/tests/fixtures")
  add_test(NAME mds_binance_loopback_integration_tests
    COMMAND mds_binance_loopback_integration_tests)
  set_tests_properties(mds_binance_loopback_integration_tests PROPERTIES
    TIMEOUT 10)
endif()

if(MDS_BUILD_EXAMPLES)
  add_executable(binance_mds_demo examples/binance_mds_demo.cpp)
  target_link_libraries(binance_mds_demo PRIVATE mds)
  target_include_directories(binance_mds_demo PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/internal)
  add_executable(mds_shm_consumer examples/shm_consumer.cpp)
  target_link_libraries(mds_shm_consumer PRIVATE mds)
  add_executable(equity_ticker_publisher examples/equity_ticker_publisher.cpp)
  target_link_libraries(equity_ticker_publisher PRIVATE mds)
  add_executable(equity_strategy_demo examples/equity_strategy_demo.cpp)
  target_link_libraries(equity_strategy_demo PRIVATE mds)
  if(MDS_BUILD_TESTS)
    add_test(NAME mds_equity_demo_integration_tests
      COMMAND bash
        ${CMAKE_CURRENT_SOURCE_DIR}/tests/equity_demo_integration.sh
        $<TARGET_FILE:equity_ticker_publisher>
        $<TARGET_FILE:equity_strategy_demo>)
    set_tests_properties(mds_equity_demo_integration_tests PROPERTIES
      TIMEOUT 8)
  endif()
endif()

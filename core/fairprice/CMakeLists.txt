cmake_minimum_required(VERSION 3.20)

if(NOT SELF_QUANT_ENABLE_MDS)
  message(FATAL_ERROR "SELF_QUANT_ENABLE_FAIRPRICE requires SELF_QUANT_ENABLE_MDS")
endif()
if(NOT SELF_QUANT_WITH_NET)
  message(FATAL_ERROR "SELF_QUANT_ENABLE_FAIRPRICE requires the net library")
endif()

find_package(yaml-cpp REQUIRED)
find_package(OpenSSL REQUIRED)
find_package(Threads REQUIRED)

if(NOT TARGET simdjson::simdjson)
  find_package(simdjson CONFIG REQUIRED)
endif()
if(NOT TARGET simdjson::simdjson)
  message(FATAL_ERROR "simdjson is required for fairprice")
endif()

find_package(PostgreSQL REQUIRED)
if(NOT PostgreSQL_FOUND)
  message(FATAL_ERROR "libpq is required for fairprice")
endif()

add_library(fairprice_core STATIC
  src/bootstrap.cpp
  src/catalog.cpp
  src/config.cpp
  src/engine.cpp
  src/history.cpp
  src/json.cpp
  src/log.cpp
  src/log_summary.cpp
  src/market_data_repository.cpp
  src/publisher.cpp
  src/recorder.cpp
  src/runtime.cpp
  src/server.cpp
  src/sqab.cpp
  src/store.cpp)
target_include_directories(fairprice_core
  PUBLIC
    $<BUILD_INTERFACE:${CMAKE_CURRENT_SOURCE_DIR}/include>)
target_link_libraries(fairprice_core
  PUBLIC
    mds
    self_quant::net
    OpenSSL::Crypto
    Threads::Threads
    simdjson::simdjson)
if(TARGET PostgreSQL::PostgreSQL)
  target_link_libraries(fairprice_core PUBLIC PostgreSQL::PostgreSQL)
else()
  target_include_directories(fairprice_core PUBLIC ${PostgreSQL_INCLUDE_DIRS})
  target_link_libraries(fairprice_core PUBLIC ${PostgreSQL_LIBRARIES})
endif()
if(TARGET yaml-cpp::yaml-cpp)
  target_link_libraries(fairprice_core PUBLIC yaml-cpp::yaml-cpp)
else()
  target_link_libraries(fairprice_core PUBLIC yaml-cpp)
endif()
if(TARGET self_quant_warnings)
  target_link_libraries(fairprice_core PRIVATE self_quant_warnings)
endif()

add_executable(fairprice cmd/main.cpp)
target_link_libraries(fairprice PRIVATE fairprice_core)
if(TARGET self_quant_warnings)
  target_link_libraries(fairprice PRIVATE self_quant_warnings)
endif()

if(BUILD_TESTING)
  add_executable(fairprice_tests
    tests/fairprice_tests.cpp
    tests/service_tests.cpp)
  target_link_libraries(fairprice_tests PRIVATE fairprice_core)
  target_include_directories(fairprice_tests PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/../mds/internal)
  target_compile_definitions(fairprice_tests PRIVATE
    FAIRPRICE_EXAMPLE_CONFIG="${CMAKE_CURRENT_SOURCE_DIR}/config/fairprice.example.yaml"
    FAIRPRICE_SHADOW_CONFIG="${CMAKE_CURRENT_SOURCE_DIR}/config/fairprice.shadow.example.yaml"
    FAIRPRICE_GOLDEN_DIR="${CMAKE_CURRENT_SOURCE_DIR}/tests/golden")
  add_test(NAME fairprice_tests COMMAND fairprice_tests)
  set_tests_properties(fairprice_tests PROPERTIES TIMEOUT 60)
endif()

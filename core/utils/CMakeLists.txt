cmake_minimum_required(VERSION 3.20)
project(self_quant_utils VERSION 1.0.0 LANGUAGES CXX)

find_package(Threads REQUIRED)

add_library(self_quant_utils STATIC
  src/decimal.cpp
  src/hardware.cpp
  src/order_book.cpp
  src/symbol.cpp
  src/timestamp.cpp
  src/wire_codec.cpp
)
add_library(self_quant::utils ALIAS self_quant_utils)
set_target_properties(self_quant_utils PROPERTIES EXPORT_NAME utils)

target_compile_features(self_quant_utils PUBLIC cxx_std_20)
target_include_directories(self_quant_utils
  PUBLIC
    $<BUILD_INTERFACE:${CMAKE_CURRENT_SOURCE_DIR}/include>
    $<INSTALL_INTERFACE:${CMAKE_INSTALL_INCLUDEDIR}>
  PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/internal
)
target_compile_options(self_quant_utils PRIVATE -Wall -Wextra -Wpedantic)
target_link_libraries(self_quant_utils PUBLIC Threads::Threads)
if(TARGET self_quant_warnings)
  target_link_libraries(self_quant_utils PRIVATE self_quant_warnings)
endif()

option(SELF_QUANT_UTILS_BUILD_TESTS "Build utils unit tests" ON)
if(SELF_QUANT_UTILS_BUILD_TESTS)
  enable_testing()
  add_executable(self_quant_utils_tests tests/utils_tests.cpp)
  target_link_libraries(self_quant_utils_tests PRIVATE self_quant_utils)
  target_include_directories(self_quant_utils_tests PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/internal)
  target_compile_options(self_quant_utils_tests PRIVATE -Wall -Wextra -Wpedantic)
  add_test(NAME self_quant_utils_tests COMMAND self_quant_utils_tests)
endif()

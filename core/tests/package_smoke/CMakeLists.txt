cmake_minimum_required(VERSION 3.20)
project(self_quant_package_smoke LANGUAGES CXX)

find_package(self_quant CONFIG REQUIRED)

add_executable(utils_package_consumer utils_consumer.cpp)
target_link_libraries(utils_package_consumer PRIVATE self_quant::utils)

if(TARGET self_quant::mds)
  add_executable(mds_package_consumer mds_consumer.cpp)
  target_link_libraries(mds_package_consumer PRIVATE self_quant::mds)
endif()

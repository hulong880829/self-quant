cmake_minimum_required(VERSION 3.20)
project(self_quant_package_smoke LANGUAGES CXX)

find_package(self_quant CONFIG REQUIRED)

add_executable(utils_package_consumer utils_consumer.cpp)
target_link_libraries(utils_package_consumer PRIVATE self_quant::utils)

if(TARGET self_quant::net)
  add_executable(net_package_consumer net_consumer.cpp)
  target_link_libraries(net_package_consumer PRIVATE self_quant::net)
endif()

if(TARGET self_quant::mds)
  add_executable(mds_package_consumer mds_consumer.cpp)
  target_link_libraries(mds_package_consumer PRIVATE self_quant::mds)
endif()

if(TARGET self_quant::mds_record)
  add_executable(mds_record_package_consumer mds_record_consumer.cpp)
  target_link_libraries(mds_record_package_consumer PRIVATE
    self_quant::mds_record)
endif()

if(TARGET self_quant::oms)
  add_executable(oms_package_consumer oms_consumer.cpp)
  target_link_libraries(oms_package_consumer PRIVATE self_quant::oms)
endif()

if(TARGET self_quant::strategyframe)
  add_executable(strategyframe_package_consumer strategyframe_consumer.cpp)
  target_link_libraries(strategyframe_package_consumer PRIVATE
    self_quant::strategyframe)
endif()

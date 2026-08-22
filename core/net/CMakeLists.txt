find_package(OpenSSL REQUIRED)
find_package(Threads REQUIRED)

add_library(self_quant_net STATIC
  src/epoll_loop.cpp
  src/http_client.cpp
  src/tcp_acceptor.cpp
  src/tcp_connector.cpp
  src/tls_websocket.cpp
  src/websocket_client.cpp
  src/websocket_codec.cpp)
add_library(self_quant::net ALIAS self_quant_net)
set_target_properties(self_quant_net PROPERTIES EXPORT_NAME net)
target_include_directories(self_quant_net
  PUBLIC
    $<BUILD_INTERFACE:${CMAKE_CURRENT_SOURCE_DIR}/include>
    $<INSTALL_INTERFACE:${CMAKE_INSTALL_INCLUDEDIR}>)
target_link_libraries(self_quant_net
  PUBLIC
    OpenSSL::SSL
    OpenSSL::Crypto
    Threads::Threads)
if(TARGET self_quant_warnings)
  target_link_libraries(self_quant_net PRIVATE self_quant_warnings)
endif()

#include "net/epoll_loop.h"

int main() {
  net::EpollLoop loop(1);
  return loop.native_handle() >= 0 ? 0 : 1;
}

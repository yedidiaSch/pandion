# Builds the demo binaries on each node — invoked by `build: make` in cluster.yaml.
# Needs libzmq (installed via `toolchain.packages: [libzmq3-dev]`).
CXXFLAGS ?= -O2 -std=c++17 -Wall
LDLIBS   ?= -lzmq

all: broker worker

broker: broker.cpp
	$(CXX) $(CXXFLAGS) -o $@ $< $(LDLIBS)

worker: worker.cpp
	$(CXX) $(CXXFLAGS) -o $@ $< $(LDLIBS)

clean:
	rm -f broker worker

.PHONY: all clean

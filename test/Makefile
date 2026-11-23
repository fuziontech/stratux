.PHONY: all clean uat_read replay_check

ROOT_DIR := $(abspath ..)
SRCS := $(filter-out uat_read.go,$(wildcard *.go))
DEST := $(patsubst %.go,%,$(SRCS))

all: $(DEST)

%: %.go
	go build -o $@ $<

uat_read: uat_read.go
	LIBRARY_PATH=$(ROOT_DIR) CGO_CFLAGS_ALLOW="-L$(ROOT_DIR)" go build -tags ignore -o $@ $<

replay_check: replay_harness
	./replay_harness -file $(ROOT_DIR)/test-data/example.dump978

clean:
	rm -f $(DEST) uat_read

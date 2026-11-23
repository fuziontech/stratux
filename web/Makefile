ifeq ($(stratuxBuild),)
stratuxBuild := $(shell git log -n 1 --pretty=%h 2>/dev/null || date +%s)
endif

ifeq ($(STRATUX_HOME),)
	STRATUX_HOME := /opt/stratux
endif

www=$(STRATUX_HOME)/www

.PHONY: all

all:
	mkdir -p $(www)
	rm -rf $(www)/css $(www)/js $(www)/img $(www)/maui $(www)/plates
	cp -r css $(www)/
	cp -r js $(www)/
	cp -r img $(www)/
	cp index.html $(www)/
	cp manifest.json $(www)/
	cp alert.wav $(www)/
	sed 's/__STRATUX_BUILD__/$(stratuxBuild)/g' sw.js > $(www)/sw.js

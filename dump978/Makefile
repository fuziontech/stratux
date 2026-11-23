CFLAGS+=-std=gnu11 -O2 -Wall -Werror -D_FORTIFY_SOURCE=2 -fstack-protector-strong -Ifec
LDFLAGS=
LIBS=-lm
CC?=gcc

LIB_OBJS=dump978_lib.o fec_lib.o decode_rs_char_lib.o init_rs_char_lib.o

.PHONY: all lib test clean

all: dump978 uat2json uat2text uat2esnt extract_nexrad

%.o: %.c *.h
	$(CC) $(CPPFLAGS) $(CFLAGS) -c $< -o $@

dump978_lib.o: dump978.c dump978.h uat.h fec.h
	$(CC) $(CPPFLAGS) $(CFLAGS) -g -fPIC -DBUILD_LIB=1 -c $< -o $@

fec_lib.o: fec.c uat.h fec.h fec/rs.h
	$(CC) $(CPPFLAGS) $(CFLAGS) -g -fPIC -c $< -o $@

decode_rs_char_lib.o: fec/decode_rs_char.c fec/decode_rs.h fec/char.h fec/rs-common.h
	$(CC) $(CPPFLAGS) $(CFLAGS) -g -fPIC -c $< -o $@

init_rs_char_lib.o: fec/init_rs_char.c fec/init_rs.h fec/char.h fec/rs-common.h
	$(CC) $(CPPFLAGS) $(CFLAGS) -g -fPIC -c $< -o $@

lib: $(LIB_OBJS)
	$(CC) -shared -o ../libdump978.so $(LIB_OBJS) $(LDFLAGS) $(LIBS)

dump978: dump978.o fec.o fec/decode_rs_char.o fec/init_rs_char.o
	$(CC) -g -o $@ $^ $(LDFLAGS) $(LIBS)

uat2json: uat2json.o uat_decode.o reader.o
	$(CC) -g -o $@ $^ $(LDFLAGS) $(LIBS)

uat2text: uat2text.o uat_decode.o reader.o
	$(CC) -g -o $@ $^ $(LDFLAGS) $(LIBS)

uat2esnt: uat2esnt.o uat_decode.o reader.o
	$(CC) -g -o $@ $^ $(LDFLAGS) $(LIBS)

extract_nexrad: extract_nexrad.o uat_decode.o reader.o
	$(CC) -g -o $@ $^ $(LDFLAGS) $(LIBS)

fec_tests: fec_tests.o fec.o fec/decode_rs_char.o fec/init_rs_char.o
	$(CC) -g -o $@ $^ $(LDFLAGS) $(LIBS)

test: fec_tests
	./fec_tests

clean:
	rm -f *~ *.o fec/*.o dump978 uat2json uat2text uat2esnt extract_nexrad fec_tests ../libdump978.so

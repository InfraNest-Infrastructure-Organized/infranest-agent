# Cuts a journal file at the end of its last object: the header names the tail object, and its own header its size.
import struct, sys
data = open(sys.argv[1], 'rb').read()
tail = struct.unpack_from('<Q', data, 136)[0]
size = struct.unpack_from('<Q', data, tail + 8)[0]
end = (tail + size + 7) & ~7
open(sys.argv[2], 'wb').write(data[:end])

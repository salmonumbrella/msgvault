#!/usr/bin/env python3
"""Build a small unencrypted Unicode PST entirely from synthetic values.

HN, BTH, PC and TC records follow the native go-pst reader's documented
MS-PST layouts. No bytes are taken from existing archive fixtures.
"""
import pathlib
import struct
import sys


def pack(fmt, *args):
    return struct.pack('<' + fmt, *args)


def heap(client, allocations, root=32):
    data = bytearray(12)
    data[2:4] = bytes([0xEC, client])
    data[4:8] = pack('I', root)
    boundaries = [12]
    for allocation in allocations:
        data.extend(allocation)
        boundaries.append(len(data))
    data[0:2] = pack('H', len(data))
    data.extend(pack('HH', len(allocations), 0))
    data.extend(pack('H' * len(boundaries), *boundaries))
    return bytes(data)


def pc(properties):
    allocations = [pack('BBBBI', 0xB5, 2, 6, 0, 64), b'']
    rows = bytearray()
    for key, kind, value in sorted(properties):
        if kind in [3, 0x0B]:
            encoded = value
        else:
            if isinstance(value, str):
                value = value.encode('utf-16le')
            allocations.append(value)
            encoded = 32 * len(allocations)
        rows.extend(pack('HHI', key, kind, encoded))
    allocations[1] = bytes(rows)
    return heap(0xBC, allocations)


def tc(columns, values):
    return tc_rows(columns, [values])


def tc_rows(columns, rows):
    # Each row is followed by its cell-existence bitmap. Variable data is
    # allocated after the TCINFO, row matrix and real row-index BTH records.
    allocations = [b'', b'', pack('BBBBI', 0xB5, 4, 4, 0, 128), pack('II', rows[0][0x67F2], 0)]
    descriptors = bytearray()
    for index, (key, kind, size) in enumerate(columns):
        descriptors.extend(pack('HHHBB', kind, key, sum(column[2] for column in columns[:index]), size, index))
    row_matrix = bytearray()
    for values in rows:
        row = bytearray()
        for key, kind, size in columns:
            value = values[key]
            if isinstance(value, str):
                allocations.append(value.encode('utf-16le'))
                value = 32 * len(allocations)
            row.extend(pack({1:'B', 2:'H', 4:'I'}[size], value))
        bitmap_offset = len(row)
        row.extend(bytes([sum(1 << (7-i) for i in range(len(columns)))]))
        row_matrix.extend(row)
    info = bytearray(22)
    info[0:2] = bytes([0x7C, len(columns)])
    info[2:10] = pack('HHHH', bitmap_offset, bitmap_offset, bitmap_offset, len(row))
    info[10:14] = pack('I', 96)
    info[14:18] = pack('I', 64)
    allocations[0] = bytes(info + descriptors)
    allocations[1] = bytes(row_matrix)
    return heap(0x7C, allocations)


def generate(path, variant="plain"):
    folder = 0x142
    if variant == "reply-chain":
        message_ids = [0x204, 0x224, 0x244]
        message_specs = [
            ("Synthetic PST root", "<root@example.test>", "", "", "Root body", False),
            ("Synthetic PST middle", "<middle@example.test>", "<root@example.test>", "<root@example.test>", "Middle body", True),
            ("Synthetic PST reply", "<reply@example.test>", "<middle@example.test>", "<root@example.test> <middle@example.test>", "Reply body", False),
        ]
    else:
        message_ids = [0x204]
        message_specs = [("Synthetic PST message", "<synthetic-pst@example.net>", "", "", "Synthetic body split across receipt-bound pages.", variant != "plain")]

    def message_headers(subject, message_id, in_reply_to, references):
        lines = [
            'From: Synthetic Sender <sender@example.net>',
            'To: Synthetic Owner <owner@example.net>',
            f'Subject: {subject}',
            'Date: Mon, 1 Jan 2001 12:00:00 +0000',
            f'Message-ID: {message_id}',
        ]
        if in_reply_to:
            lines.append(f'In-Reply-To: {in_reply_to}')
        if references:
            lines.append(f'References: {references}')
        return '\r\n'.join(lines + ['', ''])

    message_blocks = {}
    for nid, (subject, message_id, in_reply_to, references, body, has_attachment) in zip(message_ids, message_specs):
        message_blocks[nid] = pc([
            (0x001A, 0x1F, 'IPM.Note'), (0x0E07, 3, 0x10 if has_attachment else 0),
            (0x0037, 0x1F, subject), (0x0C1A, 0x1F, 'Synthetic Sender'),
            (0x0C1E, 0x1F, 'SMTP'), (0x0C1F, 0x1F, 'sender@example.net'),
            (0x1000, 0x1F, body), (0x007D, 0x1F, message_headers(subject, message_id, in_reply_to, references)),
            (0x1035, 0x1F, message_id),
        ])
    blocks = {
        0x61: pc([(2, 0x102, b''), (3, 0x102, b''), (4, 0x102, b'')]),
        0x122: pc([(0x3001, 0x1F, 'Synthetic root')]),
        0x12D: tc_rows([(0x3001, 0x1F, 4), (0x3602, 3, 4), (0x67F2, 3, 4), (0x360A, 0x0B, 1)], [{0x3001:'Synthetic Inbox',0x3602:len(message_ids),0x67F2:folder,0x360A:0}]),
        folder+12: tc_rows([(0x67F2, 3, 4)], [{0x67F2:nid} for nid in message_ids]),
    }
    blocks.update(message_blocks)
    nbt_offset, bbt_offset = 1024, 1536
    output = bytearray(2048)
    output[:4] = b'!BDN'
    output[8:12] = b'SM' + pack('H',23)
    output[224:232] = pack('Q',nbt_offset)
    output[240:248] = pack('Q',bbt_offset)
    output[512:516] = b'\0\0\0\0'  # unencrypted
    nodes, block_entries = bytearray(), bytearray()
    content_blocks = [(nid, data, None) for nid, data in sorted(blocks.items())]
    message_subnodes = {}
    if variant in ("media", "reply-chain"):
        attachment_data = b"Synthetic PST attachment" if variant == "media" else b"Synthetic intermediate attachment" + b"x" * 8192
        attachment_bid = 0x100 + 4*len(content_blocks)
        if variant == "media":
            attachment_pc_bid = attachment_bid + 4
            descriptor_bid = (attachment_bid + 8) | 2
            attachment_blocks = [
                (None, tc_rows([(0x67F2,3,4)], [{0x67F2:0x805}]), attachment_bid),
                (None, pc([(0x3701,0x102,attachment_data),(0x3704,0x1F,"synthetic.bin"),(0x3707,0x1F,"synthetic.bin"),(0x370E,0x1F,"application/octet-stream"),(0x0E20,3,len(attachment_data)),(0x3705,3,1)]), attachment_pc_bid),
                (None, pack("BBHI",2,0,2,0)+pack("QQQ",1649,attachment_bid,0)+pack("QQQ",0x805,attachment_pc_bid,0), descriptor_bid),
            ]
        else:
            descriptor_bid = (attachment_bid + 4) | 2
            attachment_pc_bid = attachment_bid + 8
            attachment_blocks = [
                (None, tc_rows([(0x67F2,3,4)], [{0x67F2:0x805}]), attachment_bid),
                (None, pack("BBHI",2,0,2,0)+pack("QQQ",1649,attachment_bid,0)+pack("QQQ",0x805,attachment_pc_bid,0), descriptor_bid),
                (None, pc([(0x3701,0x102,attachment_data),(0x3704,0x1F,"synthetic.bin"),(0x3707,0x1F,"synthetic.bin"),(0x370E,0x1F,"application/octet-stream"),(0x0E20,3,len(attachment_data)),(0x3705,3,1)]), attachment_pc_bid),
            ]
        content_blocks.extend(attachment_blocks)
        message_subnodes[message_ids[1] if variant == "reply-chain" else message_ids[0]] = descriptor_bid
    for index, block in enumerate(content_blocks):
        nid, data = block[:2]
        bid = 0x100 + 4*index
        if len(block) > 2 and block[2] is not None:
            bid = block[2]
        offset = len(output)
        if nid is not None:
            subnodes = message_subnodes.get(nid, 0)
            nodes.extend(pack('QQQII',nid,bid,subnodes,folder if nid in message_ids else 0,0))
        block_entries.extend(pack('QQHHI',bid,offset,len(data),1,0))
        output.extend(data)
        output.extend(bytes((-len(output)-16)%64))
        output.extend(pack('HHIQ',len(data),0,0,bid))
    for offset, entries, entry_size, page_type in [(nbt_offset,nodes,32,0x81),(bbt_offset,block_entries,24,0x80)]:
        output[offset:offset+len(entries)] = entries
        output[offset+488:offset+492] = bytes([len(entries)//entry_size,488//entry_size,entry_size,0])
        output[offset+496:offset+498] = bytes([page_type,page_type])
    output[184:192] = pack('Q',len(output))
    pathlib.Path(path).write_bytes(output)


if __name__ == '__main__':
    generate(sys.argv[1], sys.argv[2] if len(sys.argv)>2 else "plain")

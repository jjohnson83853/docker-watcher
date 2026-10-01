"""Frame parser behavior for Graphify attached streams."""
import struct

import graphify_stream

def _frame(stream_type, payload):
    return bytes([stream_type, 0, 0, 0]) + struct.pack(">I", len(payload)) + payload


def test_frame_parser_preserves_split_multiplexed_frames():
    parser = graphify_stream.FrameParser()
    wire = _frame(1, b"stdout error: path not found: ignored") + _frame(
        2, b"error: path not found: /private/workspace"
    )

    for byte in wire:
        parser.feed(bytes([byte]))

    assert parser.finish() == "workspace_path_missing"


def test_frame_parser_matches_only_stderr_and_prefers_path_signature():
    parser = graphify_stream.FrameParser()
    parser.feed(_frame(1, b"error: path not found: from stdout"))
    parser.feed(_frame(
        2,
        "Nothing to update or rebuild failed — check output above.\n"
        "error: path not found: /private/path".encode(),
    ))

    assert parser.finish() == "workspace_path_missing"


def test_frame_parser_classifies_exact_rebuild_signature_from_stderr():
    parser = graphify_stream.FrameParser()
    parser.feed(_frame(
        2, "Nothing to update or rebuild failed — check output above.".encode()
    ))

    assert parser.finish() == "graphify_rebuild_failed"


def test_frame_parser_marks_malformed_or_truncated_input_unavailable():
    malformed = graphify_stream.FrameParser()
    malformed.feed(bytes([3, 0, 0, 0]) + struct.pack(">I", 0))
    truncated = graphify_stream.FrameParser()
    truncated.feed(_frame(2, b"error: path not found: /partial")[:-1])

    assert malformed.finish() == "unavailable"
    assert truncated.finish() == "unavailable"


def test_frame_parser_rejects_declared_oversized_payload():
    parser = graphify_stream.FrameParser()
    parser.feed(bytes([2, 0, 0, 0]) + struct.pack(
        ">I", graphify_stream.MAX_FRAME_BYTES + 1
    ))

    assert parser.finish() == "unavailable"


def test_frame_parser_cap_counts_stdout_and_stderr_payload_bytes():
    parser = graphify_stream.FrameParser()
    cap = graphify_stream.SCAN_CAP_BYTES
    parser.feed(_frame(2, b"error: path not found: /seen\\n"))
    parser.feed(_frame(1, b"x" * (cap - len(b"error: path not found: /seen\\n"))))
    parser.feed(_frame(2, b"y"))

    assert parser.finish() == "unavailable"
def test_frame_parser_accepts_payload_exactly_at_scan_cap():
    parser = graphify_stream.FrameParser()
    parser.feed(_frame(1, b"x" * graphify_stream.SCAN_CAP_BYTES))

    assert parser.finish() == "unclassified"


def test_frame_parser_empty_stream_is_unclassified():
    assert graphify_stream.FrameParser().finish() == "unclassified"

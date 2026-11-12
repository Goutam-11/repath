def clamp(value: int, low: int, high: int) -> int:
    if value < low:
        return low
    if value > high:
        return high
    return value


def parse(text: str) -> int:
    return int(clamp(int(text), 0, 100))


def _secret() -> str:
    return "hidden"

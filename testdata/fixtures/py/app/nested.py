def outer() -> int:
    def inner(value: int) -> int:
        return value

    return inner(1)


def callback_call(fn) -> int:
    return fn(1)

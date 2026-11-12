from cart import Cart, make


def run(cart: Cart) -> int:
    return cart.total()


def run_factory() -> int:
    cart = make()
    return cart.total()

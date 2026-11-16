from cart import make
from models import Node


def run_factory() -> int:
    cart = make()
    return cart.total()


def run_child(node: Node) -> int:
    return node.parent.total()

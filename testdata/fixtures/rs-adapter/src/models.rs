pub trait Total {
    fn total(&self) -> u32;
}

pub struct Cart {
    pub count: u32,
}

impl Total for Cart {
    fn total(&self) -> u32 {
        self.count
    }
}

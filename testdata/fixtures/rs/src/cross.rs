use crate::models::Cross;

impl Cross {
    pub fn make() -> Self {
        Cross { n: 2 }
    }

    pub fn cross(&self) -> u32 {
        self.n
    }
}
